// TX holder and Dante playback-out.
//
// The always-running inferno2pipe process is receive-only (it advertises zero
// TX channels), so the unit's Dante TX side lives here: the app persistently
// holds inferno's ALSA virtual device open for playback, which keeps the TX
// channels advertised on the network in both RECORDING and PLAYBACK modes.
// Playback-out pumps ffmpeg-decoded s32le through that holder instead of
// ffmpeg's own `-f alsa` open, because inferno keeps its Dante instance in a
// process-global map - a second opener (a per-playback ffmpeg) would be a
// second instance fighting inferno2pipe for the Dante UDP ports. This mirrors
// the proven inferno-loopback.sh pi9696tx pattern (own NAME, PROCESS_ID and
// ALT_PORT), productized into the app.
//
// Until the RX side moves in-process too this means two Dante devices on the
// wire: <name> (RX, inferno2pipe) and <name>-TX (TX, this holder). TX and RX
// channel counts stay equal because both sides take the single channelCount.
package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"pi9696/alsapcm"
)

const (
	// Dante instance separation for the TX holder. inferno2pipe holds the
	// default UDP ports; only one instance can, so the holder takes its own
	// block. inferno-loopback.sh uses 10100/10200, hence 10300 here - never
	// run the loopback script while the app holds TX or the two collide.
	txAltPort   = 10300
	txProcessID = 1
	// Pump chunk size in frames; matches alsapcm's per-IO sizing so one pump
	// iteration is one device write.
	txPumpFrames = 1024
)

// txFrameWriter is the playback sink: *alsapcm.Device in production, a fake
// in tests (the dev box has no inferno ALSA device).
type txFrameWriter interface {
	Write([]int32) (int, error)
	Close() error
}

var (
	txHolder         txFrameWriter
	txHolderDevice   string // device string the holder was opened with; empty when closed
	txHolderReady    bool   // warmed up against the clock overlay; open-but-unready refuses Dante playback
	txReopenPending  bool   // audio settings moved while playing; reconcile once idle
	playbackViaDante bool   // current/last take plays through the holder, not local ALSA
	// Swappable for tests.
	openTxDevice = func(device string, rate, channels int) (txFrameWriter, error) {
		return alsapcm.OpenPlayback(device, rate, channels)
	}
)

// sanitizeDanteName maps the unit name onto Dante device-name rules
// (letters, digits, hyphen; starts with a letter; max 31 chars). Spaces and
// the dashboard's underscores become hyphens so "PI 9696_Live" still routes.
func sanitizeDanteName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "PI9696"
	}
	if c := out[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
		out = "D" + out
	}
	if len(out) > 31 {
		out = out[:31]
	}
	return strings.TrimRight(out, "-")
}

// txAlsaDevice builds the ALSA device string for the TX holder. Only numeric
// channel/rate args plus the name travel in the string: the plugin fixes TX
// channels from its config at define time and the app cannot renegotiate
// them after open, so the string pins TX_CHANNELS to the current
// channelCount (TX==RX by construction) while the deployed /etc/asound.conf
// keeps its @args indirection for everything else.
func txAlsaDevice(name string, rate, channels int) string {
	return fmt.Sprintf("inferno:NAME=%s-TX,SAMPLE_RATE=%d,TX_CHANNELS=%d,RX_CHANNELS=0,PROCESS_ID=%d,ALT_PORT=%d",
		sanitizeDanteName(name), rate, channels, txProcessID, txAltPort)
}

// ensureTxHolder reconciles the persistent TX holder with the current audio
// settings: open at boot, reopen when rate/channels/name move, warm up
// against the clock overlay. It never holds the app mutex across blocking
// ALSA IO, so callers must not hold it either - fire and forget with
// `go ensureTxHolder()`. While a take plays the holder is frozen (the pump
// owns it) and the reopen is deferred to the playback reaper via
// txReopenPending.
func ensureTxHolder() {
	mutex.Lock()
	if demoMode {
		mutex.Unlock()
		return
	}
	if currentState == StatePlaying || currentState == StatePaused {
		txReopenPending = true
		mutex.Unlock()
		return
	}
	txReopenPending = false
	rate := sampleRates[sampleRateIdx]
	channels := channelCount
	name := deviceName
	want := txAlsaDevice(name, rate, channels)
	if txHolder != nil && txHolderDevice == want {
		if txHolderReady {
			mutex.Unlock()
			return
		}
		holder := txHolder
		mutex.Unlock()
		warmupTxHolder(holder, channels)
		return
	}
	old := txHolder
	txHolder = nil
	txHolderDevice = ""
	txHolderReady = false
	if old != nil {
		old.Close()
	}
	mutex.Unlock()

	holder, err := openTxDevice(want, rate, channels)
	if err != nil {
		// No inferno ALSA device (dev box, sim, plugin not installed):
		// Dante playback is unavailable and takes fall back to local ALSA.
		// A clock-less Dante LAN is the other case, but that fails at
		// warmup, not here.
		logInfof("TX holder unavailable (%v) - Dante playback off, local fallback", err)
		return
	}
	mutex.Lock()
	if sampleRates[sampleRateIdx] != rate || channelCount != channels || deviceName != name {
		mutex.Unlock()
		holder.Close()
		mutex.Lock()
		txReopenPending = true
		mutex.Unlock()
		go ensureTxHolder()
		return
	}
	txHolder = holder
	txHolderDevice = want
	mutex.Unlock()
	warmupTxHolder(holder, channels)
}

// warmupTxHolder pushes one chunk of silence through a freshly opened holder.
// The first IO runs the plugin's prepare, which creates the Dante instance
// (advertising TX from boot) and waits for the clock overlay - up to ~5s
// with no clock, so this always runs off-mutex. Failure leaves the holder
// open but unready: Dante playback is refused with a notice until the clock
// appears, rather than silently playing out of the wrong output.
func warmupTxHolder(holder txFrameWriter, channels int) {
	zeros := make([]int32, txPumpFrames*channels)
	if _, err := holder.Write(zeros); err != nil {
		logWarnf("TX holder not ready (no clock overlay?): %v", err)
		return
	}
	mutex.Lock()
	if txHolder == holder {
		txHolderReady = true
		logInfof("TX holder ready (%s)", txHolderDevice)
	} else {
		holder.Close()
	}
	mutex.Unlock()
}

// txStatusLocked reports the Dante TX state for the UI: short fits one
// 256px OLED menu value, long suits the dashboard. Callers hold the app
// mutex (same discipline as webNoticeIfLive).
func txStatusLocked() (short, long string) {
	name := sanitizeDanteName(deviceName) + "-TX"
	switch {
	case txHolder != nil && txHolderReady:
		return "ready", "Dante TX ready (" + name + ")"
	case txHolder != nil:
		return "no clock", "Dante TX: waiting for clock"
	case demoMode:
		return "off", "Dante TX off (demo mode)"
	default:
		return "off", "Dante TX unavailable (no device)"
	}
}

// txStatusShort is the OLED form; it locks, unlike txStatusLocked.
func txStatusShort() string {
	mutex.Lock()
	defer mutex.Unlock()
	short, _ := txStatusLocked()
	return short
}

// closeTxHolder drops the persistent TX device (shutdown). All pumps are
// dead by then: playback is reaped before this runs, and a stale pump exits
// on its generation check before touching the closed handle.
func closeTxHolder() {
	mutex.Lock()
	defer mutex.Unlock()
	if txHolder != nil {
		txHolder.Close()
		txHolder = nil
		txHolderDevice = ""
		txHolderReady = false
	}
}

// dantePlaybackCmdFor decodes a take to raw s32le on stdout for the pump.
// Rate/channels are pinned to the device: validatePlaybackFile already
// refused mismatched takes, so ffmpeg never resamples silently here.
func dantePlaybackCmdFor(file string, pos time.Duration) (*exec.Cmd, io.ReadCloser, error) {
	rate := sampleRates[sampleRateIdx]
	channels := channelCount
	args := []string{"-nostdin"}
	if pos > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", pos.Seconds()))
	}
	args = append(args, "-i", file,
		"-f", "s32le", "-ac", fmt.Sprintf("%d", channels), "-ar", fmt.Sprintf("%d", rate), "-")
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, out, nil
}

// buildPlaybackCmd picks the playback sink. Callers hold the app mutex; this
// only reads holder readiness, never blocks on ALSA. Dante wins when the
// holder is ready, with local ALSA as the fallback (dev/sim/no plugin), so a
// Pi without the inferno device keeps today's behaviour exactly.
func buildPlaybackCmd(file string, pos time.Duration) (cmd *exec.Cmd, stdout io.ReadCloser, viaDante bool) {
	if !demoMode && txHolder != nil && txHolderReady {
		if c, out, err := dantePlaybackCmdFor(file, pos); err == nil {
			return c, out, true
		} else {
			logWarnf("Dante playback unavailable (%v), falling back to local ALSA", err)
		}
	}
	return playbackCmdFor(file, pos), nil, false
}

// pumpPlaybackToTx moves decoded frames from ffmpeg's stdout to the TX
// holder until its generation ends (seek handoff, stop, EOF). While paused
// it writes silence without consuming the pipe: the full pipe back-pressures
// ffmpeg to a stop, so no SIGSTOP choreography is needed and the wall-clock
// playhead machinery is untouched. A dead sink kills the decoder so the
// existing reaper drives the deck back to idle instead of stranding it.
func pumpPlaybackToTx(cmd *exec.Cmd, src io.Reader, holder txFrameWriter, channels int) {
	frameBytes := channels * 4
	tmp := make([]byte, txPumpFrames*frameBytes)
	carry := make([]byte, 0, txPumpFrames*frameBytes)
	zeros := make([]int32, txPumpFrames*channels)
	samples := make([]int32, txPumpFrames*channels)

	for {
		mutex.Lock()
		alive := playbackCmd == cmd && txHolder == holder
		paused := currentState == StatePaused
		mutex.Unlock()
		if !alive {
			return
		}
		if paused {
			if _, err := holder.Write(zeros); err != nil {
				break
			}
			continue
		}
		n, rerr := src.Read(tmp)
		if n > 0 {
			carry = append(carry, tmp[:n]...)
			failed := false
			for len(carry) >= frameBytes {
				full := len(carry) / frameBytes
				if full > txPumpFrames {
					full = txPumpFrames
				}
				chunk := carry[:full*frameBytes]
				for i := range samples[:full*channels] {
					o := i * 4
					samples[i] = int32(chunk[o]) | int32(chunk[o+1])<<8 | int32(chunk[o+2])<<16 | int32(chunk[o+3])<<24
				}
				if _, err := holder.Write(samples[:full*channels]); err != nil {
					failed = true
					break
				}
				carry = carry[full*frameBytes:]
			}
			if failed {
				break
			}
		}
		if rerr != nil {
			return
		}
	}
	logErrorf("Dante playback output failed - stopping take")
	showWebNotice("Dante playback output failed - take stopped")
	if cmd.Process != nil {
		signalTERM(cmd.Process, "playback (TX sink failed)")
	}
}
