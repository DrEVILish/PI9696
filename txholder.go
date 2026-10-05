// TX holder and inferno playback-out.
//
// The always-running inferno2pipe process is receive-only (it advertises zero
// TX channels), so the unit's inferno TX side lives here: the app persistently
// holds inferno's ALSA virtual device open for playback, which keeps the TX
// channels advertised on the network in both RECORDING and PLAYBACK modes.
// Playback-out pumps ffmpeg-decoded s32le through that holder instead of
// ffmpeg's own `-f alsa` open, because inferno keeps its device instance in a
// process-global map - a second opener (a per-playback ffmpeg) would be a
// second instance fighting inferno2pipe for the inferno UDP ports. This mirrors
// the proven inferno-loopback.sh pi9696tx pattern (own NAME, PROCESS_ID and
// ALT_PORT), productized into the app.
//
// Until the RX side moves in-process too this means two inferno devices on the
// wire: <name> (RX, inferno2pipe) and <name>-TX (TX, this holder). TX and RX
// channel counts stay equal because both sides take the single channelCount.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"pi9696/alsapcm"
)

const (
	// inferno instance separation for the TX holder. inferno2pipe holds the
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
	txHolderDevice   string // settings identity the holder was opened with; empty when closed
	txHolderReady    bool   // warmed up against the clock overlay; open-but-unready refuses inferno playback
	txReopenPending  bool   // audio settings moved while playing; reconcile once idle
	playbackViaDante bool   // current/last take plays through the holder, not local ALSA
	// Swappable for tests.
	openTxDevice = func(device string, rate, channels int) (txFrameWriter, error) {
		return alsapcm.OpenPlayback(device, rate, channels)
	}
)

// --- Single-instance (in-process RX + TX) path --------------------------
//
// The owner's invariant is one inferno instance per unit, with equal RX and
// TX channel counts both visible on the network. The historical layout ran
// two instances (inferno2pipe RX-only + this holder TX-only), which showed
// up as two devices on one IP and advertised 0 RX. alsapcm.Open opens a
// paired capture+playback handle on a single ALSA device, so one instance
// can do both. When PI9696_INPROC_RX is set, doStartInferno opens that paired
// device and an in-process reader copies captured frames into the same FIFO
// inferno2pipe used to write, leaving every downstream consumer unchanged.
//
// Gated by the env toggle while the path is validated against the bit-exact
// channel sweep; the default remains the proven inferno2pipe subprocess.

// inProcRX reports whether the single-instance in-process RX+TX path is on.
func inProcRX() bool { return os.Getenv("PI9696_INPROC_RX") != "" }

var (
	inProcRxDevice *alsapcm.Device // the paired capture+playback device, nil when stopped
	inProcRxQuit   chan struct{}   // closed to stop the capture loop
	inProcRxDone   chan struct{}   // closed by the capture loop when it has exited
	inProcRxGen    uint64          // generation tag, bumped per (re)start
)

// unifiedTxInfernoEnv is the plugin config for the single paired instance:
// equal RX and TX channels, the unit's own name (no "-TX" suffix), and the
// primary UDP ports (no ALT_PORT/PROCESS_ID override, which existed only to
// keep the TX-only holder off inferno2pipe's ports).
func applyUnifiedInfernoEnv(name string, rate, channels int) {
	os.Setenv("INFERNO_NAME", sanitizeDanteName(name))
	os.Setenv("INFERNO_SAMPLE_RATE", fmt.Sprintf("%d", rate))
	os.Setenv("INFERNO_TX_CHANNELS", fmt.Sprintf("%d", channels))
	os.Setenv("INFERNO_RX_CHANNELS", fmt.Sprintf("%d", channels))
	os.Setenv("INFERNO_TX_SOURCE_BIT_DEPTH", txSourceBitDepth)
	// Clear the TX-only holder's separation keys so the single instance binds
	// the default ports and the default process id.
	os.Unsetenv("INFERNO_ALT_PORT")
	os.Unsetenv("INFERNO_PROCESS_ID")
}

// startInProcInferno opens the paired device, publishes it as the TX holder,
// and starts the capture loop feeding fifoPath. Returns false if the device
// cannot be opened (no inferno ALSA plugin: caller falls back to InfernoFailed
// exactly as a missing inferno2pipe binary would). Runs without the app mutex.
func startInProcInferno(name string, rate, channels int, fifoPath string) bool {
	applyUnifiedInfernoEnv(name, rate, channels)
	dev, err := alsapcm.Open("inferno", rate, channels)
	if err != nil {
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
	txHolderFailed = false
	mutex.Unlock()
	go infernoRxLoop(dev, fifoPath, quit, done, gen, channels)
	// Warm up the TX side against the clock overlay, same as the TX-only
	// holder; recording is gated separately on clock sync (clocksync.go).
	go warmupTxHolder(dev, channels)
	return true
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
// ffmpeg records and monitors from, in the exact interleaved little-endian
// s32le layout inferno2pipe wrote, so the recording pipeline is unchanged.
//
// Raw O_NONBLOCK FIFO writes (not os.File) for the same reason as demoGenLoop:
// Go's poller would park a blocking write with no reader and ignore quit;
// EAGAIN keeps every iteration responsive. O_RDWR holds a read end so a write
// with no ffmpeg attached yet gets EAGAIN rather than SIGPIPE.
func infernoRxLoop(dev *alsapcm.Device, path string, quit <-chan struct{}, done chan struct{}, gen uint64, channels int) {
	nameThread(threadRxCapture)
	defer close(done)
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
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

// txInfernoEnv returns the INFERNO_* environment for the TX holder open.
// The deployed /etc/asound.conf is deliberately minimal (no @args): ALSA
// device-string arguments are rejected with "Unknown parameters", so every
// setting travels via the environment, which inferno_aoip merges as gaps
// behind the ALSA config. These are process-global, so doStartInferno
// scrubs the instance-separating keys back out of its child's environment.
func txInfernoEnv(name string, rate, channels int) map[string]string {
	return map[string]string{
		"INFERNO_NAME":                sanitizeDanteName(name) + "-TX",
		"INFERNO_SAMPLE_RATE":         fmt.Sprintf("%d", rate),
		"INFERNO_TX_CHANNELS":         fmt.Sprintf("%d", channels),
		"INFERNO_RX_CHANNELS":         "0",
		"INFERNO_PROCESS_ID":          fmt.Sprintf("%d", txProcessID),
		"INFERNO_ALT_PORT":            fmt.Sprintf("%d", txAltPort),
		"INFERNO_TX_SOURCE_BIT_DEPTH": txSourceBitDepth,
	}
}

// txSourceBitDepth tells inferno what the transmitted samples really are.
// Takes are 24-bit PCM (OutputBitsPerSample), decoded into s32 with the low
// byte zero; inferno's default (32) made it TPDF-dither them down to 24 on
// the wire, so playback changed about a quarter of all samples by 1 LSB and
// was never bit-transparent (e2e_bitperfect.py, INFERNO-UPSTREAM U3/U4).
// 24 sends them untouched.
const txSourceBitDepth = "24"

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
//
// Calls are serialized (txReconcileMu): several triggers fire together (an
// Inferno restart, a rename, a take ending), and two concurrent reopens
// each started an inferno instance on the same ports - the loser panicked
// with "address already in use" inside the plugin while the app, whose
// warm-up write still succeeded, reported TX ready.
func ensureTxHolder() {
	// In single-instance mode the paired device is owned by
	// doStartInferno/doStopInferno (RX and TX are one instance), so there is
	// no separate TX holder to reconcile; a rename or settings change goes
	// through an Inferno restart instead.
	if inProcRX() {
		return
	}
	txReconcileMu.Lock()
	defer txReconcileMu.Unlock()
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
	txHolderFailed = false
	if old != nil {
		old.Close()
		txWriteLocks.Delete(old)
	}
	mutex.Unlock()

	holder, ok := openVerifiedTxHolder(name, rate, channels)
	if !ok {
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

// txReconcileMu serializes ensureTxHolder (see there). Never taken with the
// app mutex held; ensureTxHolder takes the app mutex inside it.
var txReconcileMu sync.Mutex

// txHolderFailed: the last reopen gave up because the TX instance never
// bound its ports. Guarded by the app mutex; shown by txStatusLocked so the
// UI says so instead of reporting ready or "no device".
var txHolderFailed bool

// txPorts are the UDP ports the TX instance binds: INFERNO_ALT_PORT and the
// next three (arc, cmc, flows control, info request - inferno's
// settings.rs ALT_PORT handling).
var txPorts = []int{txAltPort, txAltPort + 1, txAltPort + 2, txAltPort + 3}

// txPortsInUse counts how many txPorts some socket on this host holds, or -1
// when that cannot be determined (the checks are then skipped). A seam:
// the test suite replaces it so it never depends on the host's sockets.
var txPortsInUse = procTxPortsInUse

var procNetUDPPath = "/proc/net/udp"

// procTxPortsInUse reads the kernel's UDP socket table rather than probing
// with a bind: a probe bind could itself steal a port from an instance that
// is starting up.
func procTxPortsInUse() int {
	data, err := os.ReadFile(procNetUDPPath)
	if err != nil {
		return -1
	}
	want := map[int]bool{}
	for _, p := range txPorts {
		want[p] = true
	}
	seen := map[int]bool{}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[min(1, len(lines)):] {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		if port, err := strconv.ParseInt(hexPort, 16, 32); err == nil && want[int(port)] {
			seen[int(port)] = true
		}
	}
	return len(seen)
}

// txPortWait bounds each wait for the TX ports to be released or bound. A
// var so tests can shrink it.
var txPortWait = 3 * time.Second

// waitTxPorts polls until exactly want of the TX ports are held, reporting
// whether that happened within txPortWait. An unknown count is success.
func waitTxPorts(want int) bool {
	deadline := time.Now().Add(txPortWait)
	for {
		n := txPortsInUse()
		if n < 0 || n == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const txOpenAttempts = 3

// openVerifiedTxHolder opens the TX device and confirms its inferno instance
// actually came up. The plugin's close only asks the previous instance to
// shut down, so a reopen straight after it raced the old sockets: the new
// instance panicked on "address already in use" and died, yet the ALSA
// handle still opened and accepted writes, so the app reported TX ready with
// nothing on the network. Now: wait for the old ports to be released, open,
// and require the new instance to hold all of them; retry, then give up
// visibly. Called without the app mutex.
//
// An open error means no inferno ALSA device at all (dev box, sim, plugin
// not installed): TX playback is unavailable and takes fall back to local
// ALSA. A clock-less network is the other case, but that fails at warmup.
func openVerifiedTxHolder(name string, rate, channels int) (txFrameWriter, bool) {
	first, last := txPorts[0], txPorts[len(txPorts)-1]
	fail := func(why string) {
		logErrorf("TX holder: %s - Inferno TX off", why)
		mutex.Lock()
		txHolderFailed = true
		showWebNotice("Inferno TX failed to start (ports busy) - see log")
		mutex.Unlock()
	}
	for attempt := 1; ; attempt++ {
		// Ports still held means the open cannot succeed - and the old
		// holder's sockets would then satisfy the "bound" check below,
		// passing a dead instance as ready. Count it as a failed attempt.
		if !waitTxPorts(0) {
			if attempt >= txOpenAttempts {
				fail(fmt.Sprintf("UDP %d-%d never released by the previous instance (%d waits of %s)", first, last, attempt, txPortWait))
				return nil, false
			}
			logWarnf("TX holder: UDP %d-%d still held after %s (attempt %d of %d), waiting again", first, last, txPortWait, attempt, txOpenAttempts)
			continue
		}
		applyTxInfernoEnv(txInfernoEnv(name, rate, channels))
		holder, err := openTxDevice("inferno", rate, channels)
		if err != nil {
			logInfof("TX holder unavailable (%v) - Inferno playback off, local fallback", err)
			return nil, false
		}
		if waitTxPorts(len(txPorts)) {
			return holder, true
		}
		holder.Close()
		if attempt >= txOpenAttempts {
			fail(fmt.Sprintf("inferno instance never bound UDP %d-%d (%d attempts)", first, last, attempt))
			return nil, false
		}
		logWarnf("TX holder: inferno instance did not bind its ports (attempt %d of %d), retrying", attempt, txOpenAttempts)
	}
}

// warmupTxHolder pushes one chunk of silence through a freshly opened holder.
// The first IO runs the plugin's prepare, which creates the inferno instance
// (advertising TX from boot) and waits for the clock overlay - up to ~5s
// with no clock, so this always runs off-mutex. Failure leaves the holder
// open but unready: inferno playback is refused with a notice until the clock
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
		startTxIdleFeeder(holder, channels, sampleRates[sampleRateIdx])
	} else {
		holder.Close()
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
	name := sanitizeDanteName(deviceName) + "-TX"
	if inProcRX() {
		// One paired instance advertises the unit's own name; there is no
		// separate "-TX" device to point the operator at.
		name = sanitizeDanteName(deviceName)
	}
	switch {
	case txHolder != nil && txHolderReady:
		return "ready", "Inferno TX ready (" + name + ")"
	case txHolder != nil:
		return "no clock", "Inferno TX: waiting for clock"
	case txHolderFailed && !demoMode:
		return "failed", "Inferno TX failed to start (ports busy) - see log"
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

// closeTxHolder drops the persistent TX device (shutdown). All pumps are
// dead by then: playback is reaped before this runs, and a stale pump exits
// on its generation check before touching the closed handle.
func closeTxHolder() {
	mutex.Lock()
	defer mutex.Unlock()
	if txHolder != nil {
		txHolder.Close()
		txWriteLocks.Delete(txHolder)
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
