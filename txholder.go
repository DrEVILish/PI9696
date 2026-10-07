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
// both take the single channelCount - plus one: the last RX and TX channel
// is always TIMECODE (timecodeChannelName), so a 64-channel unit is a
// 65-channel inferno device.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	txHolderChannels int    // the holder's channel count: audio channels + TIMECODE
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
func applyUnifiedInfernoEnv(name string, rate, channels, rxLatency int) {
	os.Setenv("INFERNO_NAME", sanitizeDanteName(name))
	// The receive latency (rxlatency.go), and where a controller's change
	// of it is handed to the app (fork 5981fee).
	os.Setenv("INFERNO_RX_LATENCY_NS", rxLatencyEnv(rxLatency))
	os.Setenv("INFERNO_LATENCY_REQUEST_PATH", infernoLatencyRequestPath)
	os.Setenv("INFERNO_SAMPLE_RATE", fmt.Sprintf("%d", rate))
	os.Setenv("INFERNO_TX_CHANNELS", fmt.Sprintf("%d", channels))
	os.Setenv("INFERNO_RX_CHANNELS", fmt.Sprintf("%d", channels))
	// The last channel each way is TIMECODE, named by the fork and not
	// renamable from a controller (fork 8f13a30).
	os.Setenv("INFERNO_FIXED_LAST_CHANNEL_NAME", timecodeChannelName)
	os.Setenv("INFERNO_TX_SOURCE_BIT_DEPTH", txSourceBitDepth)
	// Controllers show this as the device's Product Version (needs the
	// fork's PRODUCT_VERSION, 2bf6974).
	os.Setenv("INFERNO_PRODUCT_VERSION", appVersion)
	// A controller's rename request is handed to the app here (see
	// devicename.go; needs the fork's NAME_REQUEST_PATH, a67a337).
	os.Setenv("INFERNO_NAME_REQUEST_PATH", infernoNameRequestPath)
	// Saved state (channel names, subscriptions) in one fixed place, read by
	// the per-recording channel names (channelnames.go; fork 0501a56).
	os.Setenv("INFERNO_STATE_DIR", infernoStateDir)
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

// startInProcInferno opens the paired device with audioChannels plus the
// TIMECODE channel, publishes it as the TX holder, and starts the capture
// loop feeding fifoPath. Returns false if the device cannot be opened (no
// inferno ALSA plugin, ports busy): the caller leaves the server
// InfernoFailed for the retry. Runs without the app mutex.
func startInProcInferno(name string, rate, audioChannels, rxLatency int, fifoPath string) bool {
	channels := audioChannels + 1
	applyUnifiedInfernoEnv(name, rate, channels, rxLatency)
	dev, err := openPairedDevice(rate, channels)
	if err != nil {
		if !infernoPluginInstalled() {
			// Not transient: the backoff retry would log this once a
			// minute forever on a dev box or simulator. A link flap or an
			// explicit restart still tries again.
			logErrorf("in-process inferno: the inferno ALSA plugin is not installed (%v) - build and install alsa_pcm_inferno (docs/install.sh)", err)
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
	txHolderChannels = channels
	txHolderDevice = fmt.Sprintf("inferno:%s:%d:%d", sanitizeDanteName(name), rate, channels)
	txHolderReady = false
	mutex.Unlock()
	go infernoRxLoop(dev, fifoPath, quit, done, gen, channels, rate)
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
		txHolder, txHolderDevice, txHolderReady, txHolderChannels = nil, "", false, 0
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
		tcMediaForget(dev)
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
// channels counts the device's channels, TIMECODE (the last) included; the
// FIFO carries the audio channels only, or all of them while a take
// records timecode as audio (see fifoSetLayout).
//
// Raw O_NONBLOCK FIFO writes (not os.File) for the same reason as demoGenLoop:
// Go's poller would park a blocking write with no reader and ignore quit;
// EAGAIN keeps every iteration responsive. O_RDWR holds a read end so a write
// with no ffmpeg attached yet gets EAGAIN rather than SIGPIPE.
func infernoRxLoop(dev pairedDevice, path string, quit <-chan struct{}, done chan struct{}, gen uint64, channels, rate int) {
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
	// Drop, never wait: the device must be read on time whether or not
	// anything reads the FIFO (playback stands the monitor down), or the
	// TIMECODE channel stops reaching the LTC reader mid-chase.
	fifo := fifoFeed{fd: fd, ctl: rxFifoCtl, dropWhenFull: true}
	var droppedAt, backlogAt time.Time
	var maxBacklog, lastBacklog int64
	// Timing of the last iteration's steps, logged when the loop falls
	// behind: a slow step here (the read, the reader, the FIFO) is the
	// loop's own stall; none with a jump in the backlog is the media clock
	// jumping under it.
	var tTop, tRead, tFed, tWritten time.Time
	defer func() { logDebugf("in-process inferno: capture loop's largest backlog %d frames", maxBacklog) }()
	for {
		select {
		case <-quit:
			return
		default:
		}
		prevTop, prevRead, prevFed, prevWritten := tTop, tRead, tFed, tWritten
		tTop = time.Now()
		fifo.serve()
		// Read recovers capture overruns in place, so each one is a gap
		// in the take that nothing reported. Surface them, at most once
		// per infernoLogSummaryEvery.
		if msg, cur := captureXrunReport(xrunsReported, dev.Xruns()); msg != "" && time.Since(xrunsAt) >= infernoLogSummaryEvery {
			logErrorf("%s", msg)
			xrunsReported, xrunsAt = cur, time.Now()
		}
		n, err := dev.Read(frames)
		tRead = time.Now()
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
		// The TIMECODE channel to the LTC reader, before the layout drops it.
		tcFeedLTC(dev, frames[:n*channels], channels, fifo.pos, time.Now(), rate)
		fifo.pos += int64(n)
		tcNoteCapture(dev, fifo.pos, rate)
		tFed = time.Now()
		// The plugin only reports an overrun once the loop is a whole ring
		// behind; past half of one, frames it has not read yet start being
		// overwritten (a take then loses a block and repeats the next).
		if c, ok := dev.(streamClocks); ok {
			if cp := c.CaptureClock(); cp.Valid {
				backlog := cp.Hw - cp.Appl
				if backlog > maxBacklog {
					maxBacklog = backlog
				}
				if backlog > int64(txRingFrames(rate)/2) && time.Since(backlogAt) >= infernoLogSummaryEvery {
					ms := func(a, b time.Time) float64 { return float64(b.Sub(a).Microseconds()) / 1000 }
					logWarnf("in-process inferno: capture loop %d frames (%.0f ms) behind the network (was %d); last iterations: read %.1f ms, LTC %.1f ms, FIFO %.1f ms, between %.1f ms; this read %.1f ms",
						backlog, float64(backlog)*1000/float64(rate), lastBacklog,
						ms(prevTop, prevRead), ms(prevRead, prevFed), ms(prevFed, prevWritten), ms(prevWritten, tTop), ms(tTop, tRead))
					backlogAt = time.Now()
				}
				lastBacklog = backlog
			}
		}
		out := fifo.layout(frames[:n*channels], channels)
		if err := fifo.write(framesAsS32LE(out, buf), quit, 2*time.Millisecond); err != nil {
			if err != errFifoQuit {
				logErrorf("in-process inferno: FIFO write failed: %v", err)
				inProcRxFailed(gen)
			}
			return
		}
		tWritten = time.Now()
		if fifo.dropped > 0 && time.Since(droppedAt) >= infernoLogSummaryEvery {
			logDebugf("in-process inferno: FIFO full (no reader?), %d bytes not queued", fifo.dropped)
			fifo.dropped, droppedAt = 0, time.Now()
		}
	}
}

// --- The FIFO's channel layout -------------------------------------------
//
// The FIFO normally carries the audio channels only: the monitor and every
// take read channelCount channels. A take that records timecode as audio
// needs the TIMECODE channel too, and ffmpeg cannot drop one channel of
// more than 64, so instead the writer switches the FIFO to all channels for
// that take and back afterwards. A switch happens at a quiet moment (no
// reader attached: the monitor is down and the take's ffmpeg not started,
// or the take's ffmpeg has exited) and drains whatever the FIFO holds
// first, so no reader ever sees two layouts mixed. The writer answers with
// the stream position of the first frame in the new layout, which is the
// take's first sample: what its timecode stamp is computed for.

// fifoLayoutReq asks the FIFO writer to switch layout.
type fifoLayoutReq struct {
	withTC bool
	reply  chan int64 // the stream position of the first frame in the layout
}

// The writers serve these between chunks: the in-process capture loop and
// the demo generator, one each.
var (
	rxFifoCtl   = make(chan fifoLayoutReq)
	demoFifoCtl = make(chan fifoLayoutReq)
)

// fifoLayoutWait bounds how long a switch waits for the writer: one chunk
// is ~21 ms at 48 kHz.
var fifoLayoutWait = 500 * time.Millisecond

// fifoSetLayout switches the active FIFO's layout (with or without the
// TIMECODE channel) and returns the stream position the new layout starts
// at. ok is false when no writer answered (none running). Caller holds the
// app mutex (demoMode picks the writer); the writers never block on it.
func fifoSetLayout(withTC bool) (pos int64, ok bool) {
	ctl := rxFifoCtl
	if demoMode && demoFifoPath != "" {
		ctl = demoFifoCtl
	}
	r := fifoLayoutReq{withTC: withTC, reply: make(chan int64, 1)}
	select {
	case ctl <- r:
	case <-time.After(fifoLayoutWait):
		return 0, false
	}
	select {
	case pos = <-r.reply:
		return pos, true
	case <-time.After(fifoLayoutWait):
		return 0, false
	}
}

// fifoFeed is a FIFO writer's side of the layout switch.
type fifoFeed struct {
	fd     int
	ctl    chan fifoLayoutReq
	withTC bool
	pos    int64 // frames taken from the source so far
	drop   bool  // a switch happened mid-chunk: abandon the rest of it
	// dropWhenFull drops a chunk the FIFO has no room for instead of
	// waiting (the capture loop; the demo generator is paced by waiting).
	// Only whole chunks: once part of one is written, the rest follows,
	// so the stream never loses frame alignment.
	dropWhenFull bool
	dropped      int64 // bytes dropped since the last report
}

// serve applies a pending layout switch, if any.
func (f *fifoFeed) serve() bool {
	select {
	case r := <-f.ctl:
		var scratch [65536]byte
		for {
			n, err := syscall.Read(f.fd, scratch[:])
			if n <= 0 || err != nil {
				break
			}
		}
		f.withTC = r.withTC
		f.drop = true
		r.reply <- f.pos
		return true
	default:
		return false
	}
}

// layout returns the frames as the FIFO carries them now: all channels, or
// (in place) without the last.
func (f *fifoFeed) layout(frames []int32, channels int) []int32 {
	if f.withTC || channels < 2 {
		return frames
	}
	return dropLastChannel(frames, channels)
}

var errFifoQuit = errors.New("quit")

// write writes out whole, waiting out a full FIFO, until quit. A layout
// switch while waiting drains the FIFO, and the rest of this chunk (in the
// old layout) is dropped.
func (f *fifoFeed) write(out []byte, quit <-chan struct{}, wait time.Duration) error {
	f.drop = false
	// Dropping: a chunk that does not fit whole is dropped whole, so the
	// writer never waits on a part-written chunk (which stalled the loop).
	if f.dropWhenFull && !f.fits(len(out)) {
		f.serve()
		f.dropped += int64(len(out))
		return nil
	}
	for off := 0; off < len(out); {
		select {
		case <-quit:
			return errFifoQuit
		default:
		}
		w, werr := syscall.Write(f.fd, out[off:])
		if werr != nil && werr != syscall.EAGAIN {
			return werr
		}
		if werr != nil || w == 0 {
			if f.serve() {
				return nil
			}
			if f.dropWhenFull && off == 0 {
				f.dropped += int64(len(out))
				return nil
			}
			time.Sleep(wait)
			continue
		}
		off += w
	}
	return nil
}

// fits reports whether n bytes fit in the FIFO now (its size minus what it
// holds). Unknown sizes count as fitting.
func (f *fifoFeed) fits(n int) bool {
	size, _, e1 := syscall.Syscall(syscall.SYS_FCNTL, uintptr(f.fd), linuxFGetPipeSz, 0)
	var held int32
	_, _, e2 := syscall.Syscall(syscall.SYS_IOCTL, uintptr(f.fd), syscall.TIOCINQ, uintptr(unsafe.Pointer(&held)))
	if e1 != 0 || e2 != 0 {
		return true
	}
	return int(size)-int(held) >= n
}

// dropLastChannel removes the last channel of interleaved frames in place
// and returns the shortened slice.
func dropLastChannel(frames []int32, channels int) []int32 {
	n := len(frames) / channels
	keep := channels - 1
	for i := 1; i < n; i++ {
		copy(frames[i*keep:(i+1)*keep], frames[i*channels:i*channels+keep])
	}
	return frames[:n*keep]
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
// was never bit-transparent (test/interop/e2e_bitperfect.py).
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
		// Silence on the audio channels; TIMECODE carries the free-running
		// code while the timecode output is on.
		buf := make([]int32, txPumpFrames*channels)
		var tc tcIdleGen
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
			tc.fill(buf, channels, rate, holder)
			if _, err := txWrite(holder, buf); err != nil {
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
// Rate/channels are pinned to the take: validatePlaybackFile already
// refused mismatched takes (channelCount, or one more for a take that
// recorded timecode as audio), so ffmpeg never resamples silently here.
func dantePlaybackCmdFor(file string, pos time.Duration) (*exec.Cmd, io.ReadCloser, error) {
	rate := sampleRates[sampleRateIdx]
	channels := takeChannels(file)
	args := []string{"-nostdin"}
	if pos > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", pos.Seconds()))
	}
	args = append(args, "-i", file,
		"-f", "s32le", "-ac", fmt.Sprintf("%d", channels), "-ar", fmt.Sprintf("%d", rate), "-")
	cmd := exec.Command("ffmpeg", args...)
	captureStderr(cmd)
	// Not StdoutPipe: Wait closes that pipe the moment ffmpeg exits, and
	// the reaper calls Wait at once - discarding whatever ffmpeg had
	// written but the pump had not read yet (up to 64 KiB, the last 8192
	// frames of every 2-channel take; e2e_bitperfect.py). The app owns
	// this pipe: startedPlaybackPipe closes the write end once ffmpeg has
	// it, and the pump reads to the real EOF and closes the read end.
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = w
	return cmd, &playbackPipe{File: r, w: w}, nil
}

// playbackPipe is the decoder's stdout as the pump reads it.
type playbackPipe struct {
	*os.File
	w *os.File // ffmpeg's end; the app's copy is closed after Start
}

// Close closes both ends (a start that failed, or the pump done).
func (p *playbackPipe) Close() error {
	p.w.Close()
	return p.File.Close()
}

// startedPlaybackPipe releases the app's copy of the write end once ffmpeg
// has started with it, so the read end sees EOF when ffmpeg exits.
func startedPlaybackPipe(stdout io.Reader) {
	if p, ok := stdout.(*playbackPipe); ok {
		p.w.Close()
	}
}

// failedPlaybackPipe closes the pipe of a decoder that did not start.
func failedPlaybackPipe(stdout io.Reader) {
	if c, ok := stdout.(io.Closer); ok {
		c.Close()
	}
}

// takeChannels is a take's channel count: from its name, else channelCount.
// Caller holds the app mutex.
func takeChannels(file string) int {
	if m := recFilenameRe.FindStringSubmatch(filepath.Base(file)); m != nil {
		if n, err := strconv.Atoi(m[4]); err == nil && n > 0 {
			return n
		}
	}
	return channelCount
}

// txPlayout describes one playback pump's stream: the take's channels and
// the transmitter's (the audio channels plus TIMECODE), and where in the
// take and its timecode the stream starts.
type txPlayout struct {
	fileChannels int
	txChannels   int
	sampleRate   int
	startFrame   int64 // the take frame the stream starts at (a seek)
	tcRef        int64 // the take's start in samples since midnight
	tcRate       int
}

// playoutFor is the pump description for file played from pos on the
// current holder. Caller holds the app mutex.
func playoutFor(file string, pos time.Duration) txPlayout {
	sr := sampleRates[sampleRateIdx]
	ref, rate, _ := takeTimecode(file, sr)
	return txPlayout{
		fileChannels: takeChannels(file), txChannels: txHolderChannels, sampleRate: sr,
		startFrame: int64(pos.Seconds() * float64(sr)), tcRef: ref, tcRate: rate,
	}
}

// publish makes this pump the source of the playing take's timecode
// position (tcPlay): written is the next take frame it writes.
func (p txPlayout) publish(owner any, written, delay int64, paused bool) {
	tcMu.Lock()
	defer tcMu.Unlock()
	if tcPlay.owner != owner {
		tcPlay.owner = owner
		tcPlay.timeRef, tcPlay.rate, tcPlay.sr = p.tcRef, p.tcRate, p.sampleRate
	}
	tcPlay.ring = delay
	tcPlay.active = true
	tcPlay.written, tcPlay.paused, tcPlay.at = written, paused, time.Now()
}

// retire ends owner's timecode position, unless a successor took over.
func (p txPlayout) retire(owner any) {
	tcMu.Lock()
	defer tcMu.Unlock()
	if tcPlay.owner == owner {
		tcPlay.active, tcPlay.owner = false, nil
	}
}

// toTxFrames converts n decoded frames (s32le bytes, p.fileChannels each)
// into transmitter frames: the take's audio channels, then TIMECODE, which
// carries the take's own last channel when it recorded timecode as audio
// (one channel more than the audio) and silence otherwise.
func (p txPlayout) toTxFrames(src []byte, dst []int32, n int) {
	audio := p.txChannels - 1
	for f := 0; f < n; f++ {
		in := src[f*p.fileChannels*4:]
		out := dst[f*p.txChannels : (f+1)*p.txChannels]
		for c := range out {
			if c < p.fileChannels && (c < audio || p.fileChannels == p.txChannels) {
				o := c * 4
				out[c] = int32(in[o]) | int32(in[o+1])<<8 | int32(in[o+2])<<16 | int32(in[o+3])<<24
			} else {
				out[c] = 0
			}
		}
	}
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

// txWriteBarrier waits for a write to holder already in progress.
func txWriteBarrier(holder txFrameWriter) {
	m, _ := txWriteLocks.LoadOrStore(holder, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	mu.Unlock()
}

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
// which hung the app (the plugin's deadlock-prone stop path). A pump retired by a seek
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
func pumpPlaybackToTx(cmd *exec.Cmd, src io.Reader, holder txFrameWriter, p txPlayout) {
	nameThread(threadTxPump)
	// The pump owns the decoder's stdout: closing it also stops an ffmpeg
	// still writing when the pump retires early (stop, seek).
	if c, ok := src.(io.Closer); ok {
		defer c.Close()
	}
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
			// The idle feeder checks txPumpsActive before each write, but
			// one may already be in flight: let it land before the pump
			// measures where its own first write goes (the chase placed
			// its start one chunk off and had to skip again).
			txWriteBarrier(holder)
		}
	}
	defer func() {
		if counted {
			txPumpsActive.Add(-1)
		}
	}()
	// The take's timecode: generated onto TIMECODE while the output is on,
	// and published for the MTC sender and the chase.
	written := p.startFrame
	var chaseSkipped, chaseHeld int64
	var enc *ltcEncoder
	if p.sampleRate > 0 {
		enc = newLTCEncoder(tcRates[p.tcRate], p.sampleRate)
	}
	defer p.retire(cmd)
	channels := p.txChannels
	frameBytes := p.fileChannels * 4
	tmp := make([]byte, txPumpFrames*frameBytes)
	carry := make([]byte, 0, txPumpFrames*frameBytes)
	zeros := make([]int32, txPumpFrames*channels)
	samples := make([]int32, txPumpFrames*channels)

	defer pumpHalt.Delete(cmd)
	// A take that ends on its own has ffmpeg exit with its last output
	// still in the pipe (64 KiB: 8192 frames at 2 channels), and the
	// reaper clears playbackCmd before the pump has read it. Requiring
	// playbackCmd == cmd dropped that tail, so the last 8192 frames of
	// every 2-channel take went out as silence (e2e_bitperfect.py). The
	// pump now plays on to EOF unless this playback was stopped or seeked
	// (pumpHalted), another one took over, or the device changed.
	stillOurs := func() bool {
		return txHolder == holder && (playbackCmd == cmd || playbackCmd == nil) && !pumpHalted(cmd)
	}
	mutex.Lock()
	alive := stillOurs()
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
			alive = stillOurs()
			paused = currentState == StatePaused && playbackCmd == cmd
			mutex.Unlock()
		}
		if !alive {
			finishTxPump(cmd, holder, channels)
			return
		}
		if paused {
			takeOver()
			p.publish(cmd, written, txDelayFrames(holder, p.sampleRate), true)
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
				// Chasing timecode (tcchase.go): skip or hold back to
				// meet the code before writing.
				if adj := tcChaseAdjust(cmd, written, holder, max(p.sampleRate, 1)); adj > 0 {
					skip := min(adj, int64(full))
					carry = carry[skip*int64(frameBytes):]
					written += skip
					chaseSkipped += skip
					continue
				} else if adj < 0 {
					n := int(min(-adj, txPumpFrames))
					if _, err := txWrite(holder, zeros[:n*channels]); err != nil {
						failed = true
						break
					}
					chaseHeld += int64(n)
					continue
				}
				if chaseSkipped+chaseHeld > 0 {
					logInfof("Chase: met the timecode - %s %d samples (%.1f ms)", map[bool]string{true: "skipped", false: "held back"}[chaseSkipped > 0],
						chaseSkipped+chaseHeld, float64(chaseSkipped+chaseHeld)*1000/float64(max(p.sampleRate, 1)))
					chaseSkipped, chaseHeld = 0, 0
				}
				p.toTxFrames(carry[:full*frameBytes], samples, full)
				if enc != nil && tcLive.output.Load() {
					enc.fill(samples[:full*channels], channels, channels-1, p.tcRef+written)
				}
				p.publish(cmd, written, txDelayFrames(holder, p.sampleRate), false)
				if _, err := txWrite(holder, samples[:full*channels]); err != nil {
					failed = true
					break
				}
				written += int64(full)
				carry = carry[full*frameBytes:]
			}
			if failed {
				break
			}
		}
		if rerr != nil {
			logInfof("Playback output: take ended at frame %d (%d left over)", written, len(carry)/frameBytes)
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
