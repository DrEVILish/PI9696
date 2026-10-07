package main

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	if adj, w := tcChaseAdjust(owner, 0, ring), want(); math.Abs(float64(adj-w)) > 500 {
		t.Fatalf("from the take start: adjust %d, want about %d", adj, w)
	}
	if w := want(); w < sr/2 {
		t.Fatalf("the code is %d samples into the take, want at least half a second", w-ring)
	}
	if adj := tcChaseAdjust(owner, want()+sr, ring); math.Abs(float64(adj+sr)) > 500 {
		t.Fatalf("a second ahead: adjust %d, want about %d", adj, -sr)
	}
	if adj := tcChaseAdjust(owner, want()+100, ring); adj != 0 {
		t.Fatalf("2 ms off: adjust %d, want 0 (within tolerance)", adj)
	}
	if adj := tcChaseAdjust(new(int), 0, ring); adj != 0 {
		t.Fatal("a pump that is not chasing was adjusted")
	}
}

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
