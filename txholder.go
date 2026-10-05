// The in-process inferno instance and inferno playback-out.
//
// The unit runs exactly one inferno instance, inside this process: the
// inferno ALSA plugin opened as a paired capture+playback device
// (alsapcm.Open). Its capture side feeds the FIFO that record and monitor
// ffmpeg read (infernoRxLoop); its playback side is the TX holder that
// playback pumps ffmpeg-decoded s32le through. Playback never lets ffmpeg
// open the device itself: inferno keeps its instance in a process-global
// map, so a second opener would be a second instance fighting this one for
// the inferno UDP ports. RX and TX channel counts are always equal, because
// both take the single channelCount.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"pi9696/alsapcm"
)

// Pump chunk size in frames; matches alsapcm's per-IO sizing so one pump
// iteration is one device write.
const txPumpFrames = 1024

// txFrameWriter is the playback sink: the paired device in production, a
// fake in tests (the dev box has no inferno ALSA device).
type txFrameWriter interface {
	Write([]int32) (int, error)
	Close() error
}

var (
	txHolder         txFrameWriter
	txHolderDevice   string // settings identity the holder was opened with; empty when closed
	txHolderReady    bool   // warmed up against the clock overlay; open-but-unready refuses inferno playback
	playbackViaDante bool   // current/last take plays through the holder, not local ALSA
)

// pairedDevice is the in-process inferno instance: capture (Read) and
// playback (Write) on one ALSA device.
type pairedDevice interface {
	txFrameWriter
	Read([]int32) (int, error)
	Xruns() int64
}

// openPairedDevice opens the inferno plugin's paired device. A seam: the
// test suite replaces it so it never opens the host's real plugin.
var openPairedDevice = func(rate, channels int) (pairedDevice, error) {
	return alsapcm.Open("inferno", rate, channels)
}

// --- The in-process instance ---------------------------------------------

var (
	inProcRxDevice pairedDevice  // the paired capture+playback device, nil when stopped
	inProcRxQuit   chan struct{} // closed to stop the capture loop
	inProcRxDone   chan struct{} // closed by the capture loop when it has exited
	inProcRxGen    uint64        // generation tag, bumped per (re)start
)

// infernoDefaultLog is the plugin's log filter unless RUST_LOG is set.
const infernoDefaultLog = "info"

// applyUnifiedInfernoEnv sets the plugin config for the paired instance:
// equal RX and TX channels, the unit's own name and the default UDP ports.
// The deployed /etc/asound.conf is deliberately minimal (no @args: ALSA
// device-string arguments are rejected with "Unknown parameters"), so every
// setting travels via the environment, which the plugin reads at open.
func applyUnifiedInfernoEnv(name string, rate, channels int) {
	os.Setenv("INFERNO_NAME", sanitizeDanteName(name))
	os.Setenv("INFERNO_SAMPLE_RATE", fmt.Sprintf("%d", rate))
	os.Setenv("INFERNO_TX_CHANNELS", fmt.Sprintf("%d", channels))
	os.Setenv("INFERNO_RX_CHANNELS", fmt.Sprintf("%d", channels))
	os.Setenv("INFERNO_TX_SOURCE_BIT_DEPTH", txSourceBitDepth)
	// Controllers show this as the device's Product Version (needs the
	// fork's PRODUCT_VERSION, 2bf6974).
	os.Setenv("INFERNO_PRODUCT_VERSION", appVersion)
	// A controller's rename request is handed to the app here (see
	// devicename.go; needs the fork's NAME_REQUEST_PATH, a67a337).
	os.Setenv("INFERNO_NAME_REQUEST_PATH", infernoNameRequestPath)
	// The plugin's logger defaults to debug, which floods the journal (it
	// once rotated the WebUI access code away). It reads RUST_LOG once, at
	// the first open; an explicit setting in the service env still wins.
	if os.Getenv("RUST_LOG") == "" {
		os.Setenv("RUST_LOG", infernoDefaultLog)
	}
	// Never inherit a port block or process id from the service environment:
	// the unit's one instance owns the default ports.
	os.Unsetenv("INFERNO_ALT_PORT")
	os.Unsetenv("INFERNO_PROCESS_ID")
}

// startInProcInferno opens the paired device, publishes it as the TX holder,
// and starts the capture loop feeding fifoPath. Returns false if the device
// cannot be opened (no inferno ALSA plugin, ports busy): the caller leaves
// the server InfernoFailed for the retry. Runs without the app mutex.
func startInProcInferno(name string, rate, channels int, fifoPath string) bool {
	applyUnifiedInfernoEnv(name, rate, channels)
	dev, err := openPairedDevice(rate, channels)
	if err != nil {
		if !infernoPluginInstalled() {
			// Not transient: the backoff retry would log this once a
			// minute forever on a dev box or simulator. A link flap or an
			// explicit restart still tries again.
			logErrorf("in-process inferno: the inferno ALSA plugin is not installed (%v) - build and install alsa_pcm_inferno (DEPLOYMENT.md)", err)
			mutex.Lock()
			infernoNoRetry = true
			mutex.Unlock()
			return false
		}
		logErrorf("in-process inferno: cannot open paired ALSA device: %v", err)
		return false
	}
	quit := make(chan struct{})
	done := make(chan struct{})
	mutex.Lock()
	inProcRxGen++
	gen := inProcRxGen
	inProcRxDevice, inProcRxQuit, inProcRxDone = dev, quit, done
	txHolder = dev
	txHolderDevice = fmt.Sprintf("inferno:%s:%d:%d", sanitizeDanteName(name), rate, channels)
	txHolderReady = false
	mutex.Unlock()
	go infernoRxLoop(dev, fifoPath, quit, done, gen, channels)
	// Warm up the TX side against the clock overlay; recording is gated
	// separately on clock sync (clocksync.go).
	go warmupTxHolder(dev, channels)
	return true
}

// infernoPluginInstalled reports whether the inferno ALSA plugin library is
// where alsa-lib looks for it. A seam for tests.
var infernoPluginInstalled = func() bool {
	for _, pattern := range []string{
		"/usr/lib/*/alsa-lib/libasound_module_pcm_inferno.so",
		"/usr/lib/alsa-lib/libasound_module_pcm_inferno.so",
		"/usr/local/lib/alsa-lib/libasound_module_pcm_inferno.so",
	} {
		if m, _ := filepath.Glob(pattern); len(m) > 0 {
			return true
		}
	}
	return false
}

// stopInProcInferno stops the capture loop, waits for it to exit, then closes
// the paired device. The wait matters: closing an ALSA handle while a readi
// is in flight on another thread is undefined, so the loop must be out of
// Read before Close. Runs without the app mutex.
func stopInProcInferno() {
	mutex.Lock()
	quit, done, dev := inProcRxQuit, inProcRxDone, inProcRxDevice
	inProcRxQuit, inProcRxDone, inProcRxDevice = nil, nil, nil
	if txFrameWriter(dev) == txHolder {
		txHolder, txHolderDevice, txHolderReady = nil, "", false
	}
	mutex.Unlock()
	if quit != nil {
		close(quit)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			logWarnf("in-process inferno: capture loop did not exit in 2s")
		}
	}
	if dev != nil {
		txWriteLocks.Delete(txFrameWriter(dev))
		// Close waits for any read or write still inside ALSA (the capture
		// loop past its timeout, a playback pump, the TX warm-up), which
		// is what makes it safe. Bound how long the worker waits on that:
		// past the bound the close completes on its own goroutine when the
		// call returns, and the next start simply fails until then.
		closed := make(chan struct{})
		go func() {
			dev.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(inProcCloseWait):
			logErrorf("in-process inferno: device close still waiting on in-flight audio I/O after %v", inProcCloseWait)
		}
	}
}

// inProcCloseWait bounds how long stopInProcInferno waits for the paired
// device to close (a var so tests can shrink it).
var inProcCloseWait = 3 * time.Second

// infernoRxLoop copies captured frames from the paired device into the FIFO
// ffmpeg records and monitors from, as interleaved little-endian s32le.
//
// Raw O_NONBLOCK FIFO writes (not os.File) for the same reason as demoGenLoop:
// Go's poller would park a blocking write with no reader and ignore quit;
// EAGAIN keeps every iteration responsive. O_RDWR holds a read end so a write
// with no ffmpeg attached yet gets EAGAIN rather than SIGPIPE.
func infernoRxLoop(dev pairedDevice, path string, quit <-chan struct{}, done chan struct{}, gen uint64, channels int) {
	nameThread(threadRxCapture)
	defer close(done)
	// O_CLOEXEC: without it every ffmpeg started later inherits a write end
	// of the FIFO, so a monitor or take never sees EOF when this writer
	// stops and has to be SIGKILLed after the stop grace.
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		logErrorf("in-process inferno: open FIFO %s: %v", path, err)
		return
	}
	defer syscall.Close(fd)

	const chunkFrames = txPumpFrames
	frames := make([]int32, chunkFrames*channels)
	buf := make([]byte, chunkFrames*channels*4)
	var xrunsReported int64
	var xrunsAt time.Time
	for {
		select {
		case <-quit:
			return
		default:
		}
		// Read recovers capture overruns in place, so each one is a gap
		// in the take that nothing reported. Surface them, at most once
		// per infernoLogSummaryEvery.
		if msg, cur := captureXrunReport(xrunsReported, dev.Xruns()); msg != "" && time.Since(xrunsAt) >= infernoLogSummaryEvery {
			logErrorf("%s", msg)
			xrunsReported, xrunsAt = cur, time.Now()
		}
		n, err := dev.Read(frames)
		if err != nil {
			// Device closed (restart) or unrecoverable: exit. A restart
			// reopens; a real fault marks the server failed so the
			// network loop's retry rebuilds it.
			select {
			case <-quit:
			default:
				logErrorf("in-process inferno: capture read ended: %v", err)
				inProcRxFailed(gen)
			}
			return
		}
		if n == 0 {
			// No media yet (clock not valid, or a short wakeup): don't spin.
			time.Sleep(2 * time.Millisecond)
			continue
		}
		out := framesAsS32LE(frames[:n*channels], buf)
		total := len(out)
		for off := 0; off < total; {
			select {
			case <-quit:
				return
			default:
			}
			w, werr := syscall.Write(fd, out[off:total])
			if werr != nil {
				if werr == syscall.EAGAIN {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				logErrorf("in-process inferno: FIFO write failed: %v", werr)
				inProcRxFailed(gen)
				return
			}
			if w == 0 {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			off += w
		}
	}
}

// hostLittleEndian is true on every target the unit runs (arm64, amd64).
var hostLittleEndian = binary.NativeEndian.Uint16([]byte{1, 0}) == 1

// framesAsS32LE returns frames as interleaved s32le bytes, the FIFO format.
// On a little-endian host that is the frames' own memory, so no per-sample
// conversion runs: at 128ch/48kHz the old PutUint32 loop touched 6M samples
// a second on the Pi for nothing. Elsewhere it converts into scratch, which
// must hold len(frames)*4 bytes. The result aliases frames or scratch and is
// valid until either is reused.
func framesAsS32LE(frames []int32, scratch []byte) []byte {
	if len(frames) == 0 {
		return nil
	}
	if hostLittleEndian {
		return unsafe.Slice((*byte)(unsafe.Pointer(&frames[0])), len(frames)*4)
	}
	for i, v := range frames {
		binary.LittleEndian.PutUint32(scratch[i*4:], uint32(v))
	}
	return scratch[:len(frames)*4]
}

// infernoLogSummaryEvery throttles the capture loop's overrun reports.
var infernoLogSummaryEvery = 10 * time.Second

// captureXrunReport returns a log line when the device's overrun count has
// grown past what was last reported, plus the count to remember.
func captureXrunReport(reported, current int64) (string, int64) {
	if current <= reported {
		return "", reported
	}
	return fmt.Sprintf("in-process inferno: %d capture overrun(s) since the last report (%d total) - audio gaps in the input", current-reported, current), current
}

// inProcRxFailed marks the server failed when the capture loop of the
// current generation dies on its own, instead of leaving infernoState at
// "running" over a FIFO nothing feeds any more.
func inProcRxFailed(gen uint64) {
	mutex.Lock()
	defer mutex.Unlock()
	if gen == inProcRxGen && inProcRxQuit != nil && infernoState == InfernoRunning {
		infernoFailedLocked()
	}
}

// sanitizeDanteName maps the unit name onto inferno device-name rules
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

// txSourceBitDepth tells inferno what the transmitted samples really are.
// Takes are 24-bit PCM (OutputBitsPerSample), decoded into s32 with the low
// byte zero; inferno's default (32) made it TPDF-dither them down to 24 on
// the wire, so playback changed about a quarter of all samples by 1 LSB and
// was never bit-transparent (e2e_bitperfect.py, INFERNO-UPSTREAM U3/U4).
// 24 sends them untouched.
const txSourceBitDepth = "24"

// warmupTxHolder pushes one chunk of silence through a freshly opened holder.
// The first IO runs the plugin's prepare, which creates the inferno instance
// (advertising TX from boot) and waits for the clock overlay - up to ~5s
// with no clock, so this always runs off-mutex. Failure leaves the holder
// open but unready: inferno playback is refused with a notice (and another
// warm-up) until the clock appears, rather than silently playing out of the
// wrong output. The holder belongs to startInProcInferno/stopInProcInferno;
// this never closes it.
func warmupTxHolder(holder txFrameWriter, channels int) {
	zeros := make([]int32, txPumpFrames*channels)
	if _, err := holder.Write(zeros); err != nil {
		logWarnf("TX holder not ready (no clock overlay?): %v", err)
		return
	}
	mutex.Lock()
	if txHolder == holder && !txHolderReady {
		txHolderReady = true
		logInfof("TX holder ready (%s)", txHolderDevice)
		startTxIdleFeeder(holder, channels, sampleRates[sampleRateIdx])
	}
	mutex.Unlock()
}

// --- Keeping the transmitter fed between playbacks ------------------------
//
// Nothing used to write the TX device between playbacks. The plugin's
// stream clock keeps running regardless, so the first write of the next
// playback found it millions of samples behind: an underrun, and the plugin
// recovers from an underrun by restarting its transmitter, which (before
// the fork's fix) dropped every flow to every receiver. A receiver then
// heard nothing for seconds; e2e_bitperfect.py measured 0.6 s of audio out
// of a 21 s playback. Receivers also timed out on an idle transmitter and
// lost the start of each playback (REPORT F3). The idle feeder writes
// silence whenever no playback pump is running, paced by the device itself
// (a blocking write once the ring is full), so the stream never underruns
// and receivers stay connected. Owner decision 2026-10-04: transmit silence
// while idle (replaces "no TX while idle").

// txPumpsActive counts running playback pumps; the feeder yields to them.
var txPumpsActive atomic.Int32

// txIdleFeeders holds the holders that have a feeder, so warm-up can be
// called again without starting a second one.
var txIdleFeeders sync.Map // txFrameWriter -> struct{}

// txIdleFeedEnabled is a test seam: the suite turns the feeder off so its
// silence does not mix into the write sequences other TX tests assert on.
var txIdleFeedEnabled = true

// startTxIdleFeeder starts the silence feeder for holder (once per holder).
// It exits when holder stops being the current TX holder or a write fails
// (the device was closed). Caller holds the app mutex.
func startTxIdleFeeder(holder txFrameWriter, channels, rate int) {
	if !txIdleFeedEnabled || channels <= 0 || rate <= 0 {
		return
	}
	if _, loaded := txIdleFeeders.LoadOrStore(holder, struct{}{}); loaded {
		return
	}
	go func() {
		nameThread(threadTxIdle)
		defer txIdleFeeders.Delete(holder)
		zeros := make([]int32, txPumpFrames*channels)
		chunk := time.Duration(txPumpFrames) * time.Second / time.Duration(rate)
		for {
			// TryLock, never Lock: render() can hold the app mutex for
			// tens of milliseconds, longer than the device can wait.
			if mutex.TryLock() {
				current := txHolder == holder
				mutex.Unlock()
				if !current {
					return
				}
			}
			if txPumpsActive.Load() > 0 {
				time.Sleep(chunk / 8)
				continue
			}
			start := time.Now()
			if _, err := txWrite(holder, zeros); err != nil {
				return
			}
			// A write returns at once while the ring has room; pace those
			// slightly faster than real time so the ring fills up to the
			// point where writes block, then the device sets the pace.
			if el := time.Since(start); el < chunk/2 {
				time.Sleep(chunk*3/4 - el)
			}
		}
	}()
}

// txStatusLocked reports the inferno TX state for the UI: short fits one
// 256px OLED menu value, long suits the dashboard. Callers hold the app
// mutex (same discipline as webNoticeIfLive).
func txStatusLocked() (short, long string) {
	name := sanitizeDanteName(deviceName)
	switch {
	case txHolder != nil && txHolderReady:
		return "ready", "Inferno TX ready (" + name + ")"
	case txHolder != nil:
		return "no clock", "Inferno TX: waiting for clock"
	case demoMode:
		return "off", "Inferno TX off (demo mode)"
	default:
		return "off", "Inferno TX unavailable (no device)"
	}
}

// txStatusShortLocked is the OLED form. Caller holds the app mutex: its
// only caller is renderAudioMenu, under render()'s lock. (It used to take
// the mutex itself, which self-deadlocked the render tick - and with it the
// whole UI - whenever the Audio menu was on screen.)
func txStatusShortLocked() string {
	short, _ := txStatusLocked()
	return short
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
	captureStderr(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	return cmd, out, nil
}

// buildPlaybackCmd picks the playback sink. Callers hold the app mutex; this
// only reads holder readiness, never blocks on ALSA. The inferno sink wins when the
// holder is ready, with local ALSA as the fallback (dev/sim/no plugin), so a
// Pi without the inferno device keeps today's behaviour exactly.
func buildPlaybackCmd(file string, pos time.Duration) (cmd *exec.Cmd, stdout io.ReadCloser, viaDante bool) {
	if !demoMode && txHolder != nil && txHolderReady {
		if c, out, err := dantePlaybackCmdFor(file, pos); err == nil {
			return c, out, true
		} else {
			logWarnf("Inferno playback unavailable (%v), falling back to local ALSA", err)
		}
	}
	cmd = playbackCmdFor(file, pos)
	captureStderr(cmd)
	return cmd, nil, false
}

// txWriteLocks serialises writes per TX holder: a pump retiring (seek, stop)
// and its successor must never write the same ALSA handle concurrently. Per
// holder, so a write wedged on one device cannot stall another.
var txWriteLocks sync.Map // txFrameWriter -> *sync.Mutex

func txWrite(holder txFrameWriter, buf []int32) (int, error) {
	m, _ := txWriteLocks.LoadOrStore(holder, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return holder.Write(buf)
}

// txSilenceFrames covers the plugin's whole playback ring at this rate: its
// buffer is the next power of two above LatencyUs, plus one pump chunk.
func txSilenceFrames(rate int) int {
	ring := 1
	for ring < rate*alsapcm.LatencyUs/1_000_000 {
		ring <<= 1
	}
	return ring + txPumpFrames
}

// finishTxPump makes TX go silent when a take ends or is stopped (owner
// decision: silent at end/stop). The inferno plugin keeps re-sending its
// ring for as long as nothing writes, so the ring is overwritten with
// silence; before this it looped the take's last ~42 ms indefinitely. The
// stream is deliberately not stopped: the plugin's stop path
// (plugin_stop -> blocking_send under its own mutex) can block forever,
// which hung the app (see INFERNO-UPSTREAM.md). A pump retired by a seek
// leaves the device to its successor.
func finishTxPump(cmd *exec.Cmd, holder txFrameWriter, channels int) {
	// Same TryLock discipline as the pump: a blocked superseded check is a
	// stalled write and an underrun.
	gone := false
	superseded := func() bool {
		if mutex.TryLock() {
			gone = txHolder != holder || (playbackCmd != nil && playbackCmd != cmd)
			mutex.Unlock()
		}
		return gone
	}
	mutex.Lock()
	rate := sampleRates[sampleRateIdx]
	gone = txHolder != holder || (playbackCmd != nil && playbackCmd != cmd)
	mutex.Unlock()
	if gone {
		return
	}
	zeros := make([]int32, txPumpFrames*channels)
	for left := txSilenceFrames(rate); left > 0; left -= txPumpFrames {
		if superseded() {
			return
		}
		if _, err := txWrite(holder, zeros); err != nil {
			return
		}
	}
}

// pumpPlaybackToTx moves decoded frames from ffmpeg's stdout to the TX
// holder until its generation ends (seek handoff, stop, EOF). While paused
// it writes silence without consuming the pipe: the full pipe back-pressures
// ffmpeg to a stop, so no SIGSTOP choreography is needed and the wall-clock
// playhead machinery is untouched. A dead sink kills the decoder so the
// existing reaper drives the deck back to idle instead of stranding it.
func pumpPlaybackToTx(cmd *exec.Cmd, src io.Reader, holder txFrameWriter, channels int) {
	nameThread(threadTxPump)
	// The pump takes over from the idle feeder only once it has something
	// to write: counted at its first decoded chunk (or first paused
	// silence), not at start. Counting at start stopped the feeder while
	// ffmpeg was still starting (~200 ms), and that gap underran the device
	// at the start of every playback (e2e_bitperfect.py: -10974 samples).
	counted := false
	takeOver := func() {
		if !counted {
			counted = true
			txPumpsActive.Add(1)
		}
	}
	defer func() {
		if counted {
			txPumpsActive.Add(-1)
		}
	}()
	frameBytes := channels * 4
	tmp := make([]byte, txPumpFrames*frameBytes)
	carry := make([]byte, 0, txPumpFrames*frameBytes)
	zeros := make([]int32, txPumpFrames*channels)
	samples := make([]int32, txPumpFrames*channels)

	mutex.Lock()
	alive := playbackCmd == cmd && txHolder == holder
	paused := currentState == StatePaused
	mutex.Unlock()
	for {
		// Refresh the state only when the app mutex is free. Blocking on it
		// stalled the pump while render() held it (up to ~95 ms against a
		// 120 ms device buffer): an underrun 2 s into every playback, and
		// an underrun restarts the plugin's transmitter (REPORT F2,
		// e2e_bitperfect.py). A state change is seen on the next free
		// chunk, ~21 ms later at most in practice.
		if mutex.TryLock() {
			alive = playbackCmd == cmd && txHolder == holder
			paused = currentState == StatePaused
			mutex.Unlock()
		}
		if !alive {
			finishTxPump(cmd, holder, channels)
			return
		}
		if paused {
			takeOver()
			if _, err := txWrite(holder, zeros); err != nil {
				break
			}
			continue
		}
		n, rerr := src.Read(tmp)
		if n > 0 {
			takeOver()
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
				if _, err := txWrite(holder, samples[:full*channels]); err != nil {
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
			finishTxPump(cmd, holder, channels)
			return
		}
	}
	logErrorf("Inferno playback output failed - stopping take")
	showWebNotice("Inferno playback output failed - take stopped")
	if cmd.Process != nil {
		signalTERM(cmd.Process, "playback (TX sink failed)")
	}
}
