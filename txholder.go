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
	"os"
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
	// Stop drops queued frames and halts transmission until the next Write.
	Stop() error
	Close() error
}

var (
	txHolder         txFrameWriter
	txHolderDevice   string // settings identity the holder was opened with; empty when closed
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

// txInfernoEnv returns the INFERNO_* environment for the TX holder open.
// The deployed /etc/asound.conf is deliberately minimal (no @args): ALSA
// device-string arguments are rejected with "Unknown parameters", so every
// setting travels via the environment, which inferno_aoip merges as gaps
// behind the ALSA config. These are process-global, so doStartInferno
// scrubs the instance-separating keys back out of its child's environment.
func txInfernoEnv(name string, rate, channels int) map[string]string {
	return map[string]string{
		"INFERNO_NAME":        sanitizeDanteName(name) + "-TX",
		"INFERNO_SAMPLE_RATE": fmt.Sprintf("%d", rate),
		"INFERNO_TX_CHANNELS": fmt.Sprintf("%d", channels),
		"INFERNO_RX_CHANNELS": "0",
		"INFERNO_PROCESS_ID":  fmt.Sprintf("%d", txProcessID),
		"INFERNO_ALT_PORT":    fmt.Sprintf("%d", txAltPort),
	}
}

// applyTxInfernoEnv presses the TX settings into the process environment
// ahead of the holder open (the plugin reads them per open at define time).
// Callers hold no locks; os.Setenv is process-global but the only other
// consumer is the inferno2pipe child, whose environment is scrubbed.
func applyTxInfernoEnv(env map[string]string) {
	for k, v := range env {
		os.Setenv(k, v)
	}
}

// scrubbedInfernoEnv returns the environment for the inferno2pipe child:
// the TX holder's instance-separating keys must not leak into it, or the
// pipe server would move off the default ports (ALT_PORT), collide IDs
// (PROCESS_ID), or misread its channel counts. RATE/NAME travel as explicit
// per-child values appended by the caller, never inherited.
func scrubbedInfernoEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch key {
		case "INFERNO_ALT_PORT", "INFERNO_PROCESS_ID", "INFERNO_TX_CHANNELS", "INFERNO_RX_CHANNELS":
			continue
		}
		out = append(out, kv)
	}
	return out
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
	// Identity is settings-derived (the device itself is always bare
	// "inferno"; the per-instance values travel via INFERNO_* env).
	want := fmt.Sprintf("inferno:%s:%d:%d", sanitizeDanteName(name), rate, channels)
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

	applyTxInfernoEnv(txInfernoEnv(name, rate, channels))
	holder, err := openTxDevice("inferno", rate, channels)
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
	// Idle sends nothing (owner decision): the warmup proved the clock, it
	// must not leave the transmitter running.
	if err := holder.Stop(); err != nil {
		logWarnf("TX holder stop after warmup: %v", err)
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

// finishTxPump makes TX go silent when a take ends or is stopped, and stops
// transmitting so an idle holder sends nothing. Without it the plugin keeps
// re-sending its last ring (~42 ms of the take's end) for as long as the
// holder sits idle. At EOF one buffer of silence goes first so the take's
// final frames still leave the device. A pump retired by a seek handoff
// leaves the device alone: the new pump owns it.
func finishTxPump(cmd *exec.Cmd, holder txFrameWriter, channels int, eof bool) {
	mutex.Lock()
	rate := sampleRates[sampleRateIdx]
	mutex.Unlock()
	if eof {
		tail := (rate*alsapcm.LatencyUs/1_000_000 + txPumpFrames - 1) / txPumpFrames
		zeros := make([]int32, txPumpFrames*channels)
		for i := 0; i < tail; i++ {
			if _, err := holder.Write(zeros); err != nil {
				break
			}
		}
	}
	mutex.Lock()
	defer mutex.Unlock()
	if txHolder != holder || (playbackCmd != nil && playbackCmd != cmd) {
		return
	}
	if err := holder.Stop(); err != nil {
		logWarnf("TX stop after playback: %v", err)
	}
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
			finishTxPump(cmd, holder, channels, false)
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
			finishTxPump(cmd, holder, channels, true)
			return
		}
	}
	logErrorf("Dante playback output failed - stopping take")
	showWebNotice("Dante playback output failed - take stopped")
	if cmd.Process != nil {
		signalTERM(cmd.Process, "playback (TX sink failed)")
	}
}
