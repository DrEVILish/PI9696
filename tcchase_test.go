package main

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"pi9696/alsapcm"
	"pi9696/hardware"
)

// feedLockedLTC feeds 0.5 s of LTC from start, ending now, so the LTC
// input reads as locked at start + 0.5 s.
func feedLockedLTC(start Timecode) {
	const sr, chunk = 48000, 1024
	now := time.Now()
	n := sr / 2 / chunk * chunk
	stream := new(int)
	for pos := 0; pos < n; pos += chunk {
		at := now.Add(-time.Duration(n-pos-chunk) * time.Second / sr)
		tcFeedLTC(stream, ltcStream(start, sr, 3, int64(pos), chunk), 3, int64(pos), at, sr)
	}
}

func TestChaseAdjust(t *testing.T) {
	setTimecodeSettings(t, tcSourceLTC, tcRecordMeta, false)
	const sr = 48000
	r := tcRates[tcRateDefault]
	// The take starts at 10:00:00:00; the code is at 10:00:00:00 + 0.5 s.
	feedLockedLTC(Timecode{10, 0, 0, 0})
	owner := new(int)
	tcMu.Lock()
	tcChaseLive.cmd, tcChaseLive.ref, tcChaseLive.sr = owner, samplesFromFrames((Timecode{10, 0, 0, 0}).frames(r), r, sr), sr
	tcMu.Unlock()
	t.Cleanup(func() { tcSetChaseLive(nil) })
	const ring = 8192
	start := samplesFromFrames((Timecode{10, 0, 0, 0}).frames(r), r, sr)
	// Where the code wants the pump now (it moves on while the test runs).
	want := func() int64 {
		in := tcInputNow(time.Now())
		return int64(tcSecondsOfFrames(in.frames, r)*sr) - start + ring
	}
	// Behind: skip up to the code; ahead: hold back; close: leave alone.
	if adj, w := tcChaseAdjust(owner, 0, ringHolder{ring}, sr), want(); math.Abs(float64(adj-w)) > 500 {
		t.Fatalf("from the take start: adjust %d, want about %d", adj, w)
	}
	if w := want(); w < sr/2 {
		t.Fatalf("the code is %d samples into the take, want at least half a second", w-ring)
	}
	if adj := tcChaseAdjust(owner, want()+sr, ringHolder{ring}, sr); math.Abs(float64(adj+sr)) > 500 {
		t.Fatalf("a second ahead: adjust %d, want about %d", adj, -sr)
	}
	if adj := tcChaseAdjust(owner, want()+100, ringHolder{ring}, sr); adj != 0 {
		t.Fatalf("2 ms off: adjust %d, want 0 (within tolerance)", adj)
	}
	if adj := tcChaseAdjust(new(int), 0, ringHolder{ring}, sr); adj != 0 {
		t.Fatal("a pump that is not chasing was adjusted")
	}
}

// ringHolder reports a fixed playback delay.
type ringHolder struct{ d int64 }

func (h ringHolder) PlaybackDelay() int64 { return h.d }

// pacedLTCDevice is ltcDevice whose writes take real time, like the
// plugin once its ring is full.
type pacedLTCDevice struct{ *ltcDevice }

func (d pacedLTCDevice) Write(b []int32) (int, error) {
	n, err := d.ltcDevice.Write(b)
	time.Sleep(time.Duration(len(b)/d.channels) * time.Second / time.Duration(d.rate))
	return n, err
}

// An armed take starts playing when the code rolls inside it, from the
// position the code names, and stays on it; the operator's Stop disarms.
func TestChasePlaysInSync(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	initTestHardware(t)
	saveTxGlobals(t)
	useFakeInferno(t)
	setTimecodeSettings(t, tcSourceLTC, tcRecordMeta, false)
	openPairedDevice = func(rate, channels int) (pairedDevice, error) {
		return pacedLTCDevice{&ltcDevice{channels: channels, rate: rate, done: make(chan struct{})}}, nil
	}
	mutex.Lock()
	demoMode = false
	sampleRateIdx, channelCount = 1, 2
	currentState = StateIdle
	oSel := selectedPlayback
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		tcDisarmLocked("")
		selectedPlayback = oSel
		mutex.Unlock()
	})
	t.Cleanup(stopInfernoAndWait)

	// A 6 s take whose timecode starts at 09:59:59:00, a second before
	// the code the device sends (10:00:00:00 from its first sample).
	dir := t.TempDir()
	take := filepath.Join(dir, "chase_20261007_095959_ch2_48kHz.wav")
	if o, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=f=440:d=6", "-ac", "2",
		"-c:a", "pcm_s24le", take).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, o)
	}
	r := tcRates[tcRateDefault]
	ref := samplesFromFrames((Timecode{9, 59, 59, 0}).frames(r), r, 48000)
	mutex.Lock()
	snapshotRecordingChannels(take, 2, &recTimecode{Start: "09:59:59:00", Rate: "25", Source: "LTC", TimeReference: ref, SampleRate: 48000})
	selectedPlayback = take
	mutex.Unlock()

	startDone := make(chan struct{})
	infernoReqCh <- infernoRequest{cmd: infernoCmdStart, done: startDone}
	<-startDone
	waitFor(t, 3*time.Second, "TX ready", func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		return txHolderReady
	})

	mutex.Lock()
	err := tcArmLocked()
	mutex.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the chase to start playback", func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		return currentState == StatePlaying && tcChase.state == "chasing"
	})
	// Let the pump meet the code, then compare the playhead with it.
	time.Sleep(1500 * time.Millisecond)
	in := tcInputNow(time.Now())
	mutex.Lock()
	head, ok := tcChasePlayhead(time.Now())
	target := tcChaseTarget(in)
	mutex.Unlock()
	if !ok || !in.locked {
		t.Fatalf("no playhead (%v) or no lock (%+v)", ok, in)
	}
	if d := head - target; math.Abs(d) > 0.03 {
		t.Fatalf("playhead %.3f s, code says %.3f s: %+.1f ms off", head, target, d*1000)
	}
	if target < 1.5 {
		t.Fatalf("code position in the take %.3f s, expected past 1.5 s (started a second in)", target)
	}

	onButtonPress(hardware.StopButton)
	waitFor(t, 5*time.Second, "stop", func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		return currentState == StateIdle && playbackCmd == nil
	})
	mutex.Lock()
	armed := tcChase.armed
	mutex.Unlock()
	if armed {
		t.Fatal("still armed after the operator's Stop")
	}
	os.Remove(channelsSidecar(take))
}

func TestArmNeedsSourceAndIdle(t *testing.T) {
	initTestHardware(t)
	setTimecodeSettings(t, tcSourceOff, tcRecordMeta, false)
	mutex.Lock()
	defer mutex.Unlock()
	if err := tcArmLocked(); err == nil {
		t.Fatal("armed with no timecode source")
	}
}

// clockDev reports fixed clock points: the capture stream started at media
// sample capStart and the playback stream at playStart; both points are
// taken at media sample now (wall time at).
type clockDev struct {
	capStart, playStart, now int64
	capAppl, playAppl        int64
	at                       time.Time
}

func (c *clockDev) CaptureClock() alsapcm.ClockPoint {
	return alsapcm.ClockPoint{Hw: c.now - c.capStart, Appl: c.capAppl, At: c.at, Valid: true}
}

func (c *clockDev) PlaybackClock() alsapcm.ClockPoint {
	return alsapcm.ClockPoint{Hw: c.now - c.playStart, Appl: c.playAppl, At: c.at, Valid: true}
}

// On the media clock the chase compares the next transmitted frame with the
// code at the same media instant, to the sample, and corrects past 1 ms.
func TestChaseAdjustOnTheMediaClock(t *testing.T) {
	setTimecodeSettings(t, tcSourceLTC, tcRecordMeta, false)
	const sr = 48000
	r := tcRates[tcRateDefault]
	// Capture began at media sample 1000, playback at 5000 (they open
	// separately). The capture loop's stream position is 300 ahead of the
	// device's appl count (an earlier xrun reset it, say).
	dev := &clockDev{capStart: 1000, playStart: 5000, now: 100000, capAppl: 98000, playAppl: 100500, at: time.Now()}
	tcNoteCapture(dev, 98300, sr)
	t.Cleanup(func() { tcMediaForget(dev) })
	// LTC from 10:00:00:00 at capture stream position 0, fed as the device.
	start := Timecode{10, 0, 0, 0}
	tcMu.Lock()
	tcLTC.dec, tcLTC.stream, tcLTC.sr, tcLTC.next = newLTCDecoder(sr), any(dev), sr, 0
	tcMu.Unlock()
	feed := ltcStream(start, sr, 3, 0, sr*22/10)
	tcMu.Lock()
	tcLTC.dec.feed(feed, 3, 2, func(f ltcDecoded) {
		tcLTC.has, tcLTC.run, tcLTC.rate = true, tcLTC.run+1, tcRateDefault
		tcLTC.nextFrames, tcLTC.endPos = f.tc.frames(r)+1, f.endPos
	})
	tcMu.Unlock()
	// The next write starts at playback appl 100500. Playback hw is 95000
	// (5500 queued), so it goes out at media sample 105500, where capture
	// hw is 104500: capture appl 104500, stream position 104800 (+300).
	// The LTC captured there was sent inferno's receive latency (10 ms,
	// 480 samples) earlier, so the code on the wire then is the code
	// captured at 105280.
	const onWire = 104800 + 480
	tcMu.Lock()
	x, ok := tcRxPosOfTxLocked(dev.playAppl)
	tcMu.Unlock()
	if !ok || math.Abs(x-onWire) > 0.01 {
		t.Fatalf("transmit appl %d maps to capture position %.2f, want %d", dev.playAppl, x, onWire)
	}
	owner := new(int)
	ref := samplesFromFrames(start.frames(r), r, sr)
	tcMu.Lock()
	tcChaseLive.cmd, tcChaseLive.ref, tcChaseLive.sr = owner, ref, sr
	tcMu.Unlock()
	t.Cleanup(func() { tcSetChaseLive(nil) })
	// The take starts at 10:00:00:00, so take frame onWire belongs on the
	// wire next.
	if adj := tcChaseAdjust(owner, onWire-40, dev, sr); adj != 0 {
		t.Fatalf("0.8 ms off: adjust %d, want 0", adj)
	}
	if adj := tcChaseAdjust(owner, onWire-100, dev, sr); adj != 100 {
		t.Fatalf("100 samples behind: adjust %d, want 100", adj)
	}
	if adj := tcChaseAdjust(owner, onWire+60, dev, sr); adj != -60 {
		t.Fatalf("60 samples ahead: adjust %d, want -60", adj)
	}
}

// With the option on, the code starting to roll or jumping back re-anchors
// the take's start to it; off, the take's own timecode applies.
func TestChaseRestartAnchors(t *testing.T) {
	initTestHardware(t)
	setTimecodeSettings(t, tcSourceLTC, tcRecordMeta, false)
	const sr = 48000
	r := tcRates[tcRateDefault]
	mutex.Lock()
	oState, oRec, oRestart := currentState, isRecording, tcRestartOn
	// Busy: the chase decides but never starts playback here.
	currentState, isRecording, tcRestartOn = StateRecording, true, true
	tcChase.armed, tcChase.sr, tcChase.dur = true, sr, time.Hour
	tcChase.ref, tcChase.takeRef, tcChase.anchored, tcChase.lastLocked = 7, 7, false, false
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		currentState, isRecording, tcRestartOn = oState, oRec, oRestart
		tcChase.armed, tcChase.anchored = false, false
		mutex.Unlock()
		tcSetChaseLive(nil)
	})
	at := func(tc Timecode) tcReading {
		return tcReading{source: "LTC", frames: float64(tc.frames(r)), rate: tcRateDefault, locked: true}
	}
	samples := func(tc Timecode) int64 { return samplesFromFrames(tc.frames(r), r, sr) }
	step := func(rd tcReading) int64 {
		mutex.Lock()
		defer mutex.Unlock()
		tcChaseStepLocked(rd, time.Now())
		return tcChase.ref
	}
	if ref := step(at(Timecode{1, 0, 0, 0})); ref != samples(Timecode{1, 0, 0, 0}) {
		t.Fatalf("rolling: take start anchored at %d, want 01:00:00:00", ref)
	}
	if ref := step(at(Timecode{1, 0, 5, 0})); ref != samples(Timecode{1, 0, 0, 0}) {
		t.Fatal("moving forward re-anchored")
	}
	if ref := step(at(Timecode{0, 59, 0, 0})); ref != samples(Timecode{0, 59, 0, 0}) {
		t.Fatal("a jump back did not re-anchor")
	}
	step(tcReading{source: "LTC"}) // stopped
	if ref := step(at(Timecode{2, 0, 0, 0})); ref != samples(Timecode{2, 0, 0, 0}) {
		t.Fatal("rolling again did not re-anchor")
	}
	mutex.Lock()
	tcRestartOn = false
	mutex.Unlock()
	if ref := step(at(Timecode{2, 0, 0, 1})); ref != 7 {
		t.Fatalf("option off: take start %d, want its own timecode (7)", ref)
	}
}

// The playback position comes in up to a packet ahead of the media clock:
// the offset estimate is the largest recent one, and a stream restart (a
// jump far beyond packet jitter) starts the history over.
func TestStreamOffsetTakesTheRunningMaximum(t *testing.T) {
	tcMu.Lock()
	defer tcMu.Unlock()
	tcMedia.dHist, tcMedia.dNext = [tcOffsetWindow]tcOffsetSample{}, 0
	now := time.Now()
	const sr = 48000
	// True offset 1000; each estimate is 0..47 samples short (a 48-sample
	// packet's phase).
	var got float64
	for i := 0; i < 100; i++ {
		got = tcOffsetMaxLocked(1000-float64((i*29)%48), now.Add(time.Duration(i)*time.Millisecond), sr)
	}
	if got != 1000 {
		t.Fatalf("offset %v, want the true 1000", got)
	}
	// Old estimates age out.
	if got = tcOffsetMaxLocked(990, now.Add(3*time.Second), sr); got != 990 {
		t.Fatalf("after 2 s only the new estimate counts: %v", got)
	}
	// A restart: a new offset far away replaces the history at once.
	if got = tcOffsetMaxLocked(50000, now.Add(3*time.Second+time.Millisecond), sr); got != 50000 {
		t.Fatalf("after a restart: %v", got)
	}
}
