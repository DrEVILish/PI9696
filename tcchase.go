package main

import (
	"fmt"
	"math"
	"path/filepath"
	"time"
)

// Chase: play the selected take in sync with incoming timecode.
//
// Arming (Timecode menu, dashboard, /api/timecode/arm) picks the take Play
// would start. From then on the unit follows the selected input: when it
// locks inside the take, playback starts at the take position its
// timecode names; when the code stops or leaves the take, playback stops
// and the take stays armed for the next roll. A jump in the code
// relocates. Any transport action of the operator's disarms.
//
// Sync is held by the inferno playback pump: before each chunk it compares
// the take frame it is about to write with the frame the timecode says
// will be heard when that chunk plays (now plus the transmitter's ring),
// and skips or inserts samples when they differ by more than
// tcChaseTolerance - which aligns the start to the sample and absorbs a
// source that drifts against the network clock. The chase loop measures
// the remaining drift and logs it.

// tcChaseTolerance is the error the pump lets stand before correcting.
const tcChaseTolerance = 10 * time.Millisecond

// tcChaseRelocate: a playhead this far off the code is a jump, not drift:
// restart at the new position.
const tcChaseRelocate = time.Second

// tcChaseSummaryEvery is how often a chasing take logs its drift.
var tcChaseSummaryEvery = 10 * time.Second

// tcChase is the chase state (app mutex).
var tcChase struct {
	armed bool
	file  string
	ref   int64 // the take's start, samples since midnight at sr
	rate  int
	sr    int
	dur   time.Duration
	cmd   any    // the playback the chase started; nil none
	state string // for the UIs
	gen   int    // bumped per arm: a loop of an older arm exits

	// drift statistics since the last summary
	n           int
	sum, maxAbs float64 // seconds
	since       time.Time
	warnedAt    time.Time
}

// tcChaseLive is what the pump reads per chunk (tcMu): which playback is
// chasing and where its take starts.
var tcChaseLive struct {
	cmd any
	ref int64
	sr  int
}

// tcArmLocked arms the take Play would start. Caller holds the app mutex.
func tcArmLocked() error {
	if tcSourceIdx == tcSourceOff {
		return fmt.Errorf("choose a timecode source first")
	}
	if isRecording || (currentState != StateIdle && currentState != StateIdleBrowse) {
		return fmt.Errorf("stop the current recording or playback first")
	}
	file := playbackSourceLocked()
	if file == "" {
		return fmt.Errorf("no take to arm")
	}
	if err := validatePlaybackFile(file); err != nil && !demoMode {
		return err
	}
	sr := sampleRates[sampleRateIdx]
	ref, rate, _ := takeTimecode(file, sr)
	tcDisarmLocked("")
	tcChase.armed, tcChase.file, tcChase.ref, tcChase.rate, tcChase.sr = true, file, ref, rate, sr
	tcChase.dur = playbackFileDuration(file)
	tcChase.cmd = nil
	tcChase.gen++
	tcChase.state = "armed"
	r := tcRates[rate]
	start := timecodeAt(framesFromSamples(ref, r, sr), r).format(r)
	logInfof("Chase: armed %s (starts at %s, %s) - waiting for %s", filepath.Base(file), start, formatDuration(tcChase.dur), tcSourceNames[tcSourceIdx])
	go tcChaseLoop(tcChase.gen)
	return nil
}

// tcDisarmLocked disarms, stopping a playback the chase started. why ""
// logs nothing. Caller holds the app mutex.
func tcDisarmLocked(why string) {
	if !tcChase.armed {
		return
	}
	tcChaseStopPlaybackLocked()
	tcChase.armed, tcChase.state = false, ""
	tcChase.gen++
	if why != "" {
		logInfof("Chase: disarmed (%s)", why)
	}
}

// tcUserTransportLocked is called on every operator transport action: the
// operator takes over from the chase. Caller holds the app mutex.
func tcUserTransportLocked() {
	if tcChase.armed {
		// The operator's own stop/pause applies to the playback; the
		// chase only lets go of it.
		tcChase.cmd = nil
		tcSetChaseLive(nil)
		tcDisarmLocked("transport taken over")
	}
}

func tcSetChaseLive(cmd any) {
	tcMu.Lock()
	tcChaseLive.cmd, tcChaseLive.ref, tcChaseLive.sr = cmd, tcChase.ref, tcChase.sr
	tcMu.Unlock()
}

// tcChaseStopPlaybackLocked stops the playback the chase started, if it is
// still the current one.
func tcChaseStopPlaybackLocked() {
	if tcChase.cmd != nil && playbackCmd != nil && any(playbackCmd) == tcChase.cmd {
		stopPlayback()
	}
	tcChase.cmd = nil
	tcSetChaseLive(nil)
}

// tcChaseLoop drives an armed take every 20 ms until disarmed.
func tcChaseLoop(gen int) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		r := tcInputNow(time.Now())
		mutex.Lock()
		if tcChase.gen != gen || !tcChase.armed {
			mutex.Unlock()
			return
		}
		tcChaseStepLocked(r, time.Now())
		mutex.Unlock()
	}
}

// tcChaseTarget is the take position the input names now (seconds; may be
// outside the take).
func tcChaseTarget(r tcReading) float64 {
	return tcSecondsOfFrames(r.frames, tcRates[r.rate]) - float64(tcChase.ref)/float64(tcChase.sr)
}

// tcChaseStepLocked is one chase decision. Caller holds the app mutex.
func tcChaseStepLocked(r tcReading, now time.Time) {
	c := &tcChase
	playing := c.cmd != nil && playbackCmd != nil && any(playbackCmd) == c.cmd
	if c.cmd != nil && !playing {
		// The take played out (or failed): stay armed for the next roll.
		c.cmd = nil
		tcSetChaseLive(nil)
	}
	if !r.locked {
		if playing {
			logInfof("Chase: %s stopped - stopping playback", r.source)
			tcChaseStopPlaybackLocked()
		}
		c.state = "armed - waiting for " + tcSourceNames[tcSourceIdx]
		return
	}
	target := tcChaseTarget(r)
	rate := tcRates[r.rate]
	label := timecodeAt(int64(r.frames), rate).format(rate)
	if target < 0 || (c.dur > 0 && target >= c.dur.Seconds()) {
		if playing && target >= c.dur.Seconds() {
			// Let the take end on its own at EOF.
			c.state = "playing out"
			return
		}
		if playing {
			logInfof("Chase: %s at %s is outside the take - stopping playback", r.source, label)
			tcChaseStopPlaybackLocked()
		}
		startTC := timecodeAt(framesFromSamples(c.ref, rate, c.sr), rate).format(rate)
		if target < 0 {
			c.state = "armed - take starts at " + startTC
		} else {
			c.state = "armed - code is past the take"
		}
		return
	}
	if !playing {
		if currentState != StateIdle && currentState != StateIdleBrowse || isRecording || playbackCmd != nil {
			c.state = "armed - transport busy"
			return
		}
		// ffmpeg takes a moment to start; the pump aligns the first chunk
		// to the code exactly, so the start position only has to be near.
		pos := time.Duration(target * float64(time.Second))
		if !startPlaybackFrom(c.file, pos) {
			tcDisarmLocked("playback could not start")
			return
		}
		c.cmd = playbackCmd
		tcSetChaseLive(playbackCmd)
		c.n, c.sum, c.maxAbs, c.since = 0, 0, 0, now
		c.state = "chasing"
		logInfof("Chase: %s locked at %s - playing %s from %s", r.source, label, filepath.Base(c.file), formatDuration(pos))
		return
	}
	// Playing: measure the drift between the playhead and the code.
	head, ok := tcChasePlayhead(now)
	if !ok {
		return
	}
	drift := head - target
	if math.Abs(drift) >= tcChaseRelocate.Seconds() {
		logWarnf("Chase: %s jumped to %s (playhead %+.3f s off) - relocating", r.source, label, drift)
		tcChaseStopPlaybackLocked()
		c.state = "relocating"
		return
	}
	c.state = "chasing"
	c.n++
	c.sum += drift
	c.maxAbs = math.Max(c.maxAbs, math.Abs(drift))
	frameSec := tcSecondsOfFrames(1, rate)
	if math.Abs(drift) > frameSec && now.Sub(c.warnedAt) > time.Second {
		logWarnf("Chase: drift %+.1f ms (%.2f frames) at %s", drift*1000, drift/frameSec, label)
		c.warnedAt = now
	}
	if now.Sub(c.since) >= tcChaseSummaryEvery && c.n > 0 {
		logInfof("Chase: drift over %s: mean %+.2f ms, max %.2f ms (%d samples) at %s",
			now.Sub(c.since).Round(time.Second), c.sum/float64(c.n)*1000, c.maxAbs*1000, c.n, label)
		c.n, c.sum, c.maxAbs, c.since = 0, 0, 0, now
	}
}

// tcChasePlayhead is the take position heard now (seconds): the pump's
// count on the inferno path, the wall-clock playhead otherwise.
func tcChasePlayhead(now time.Time) (float64, bool) {
	if f, _, _, sr, ok := tcPlayPosition(now); ok && playbackViaDante {
		return f / float64(sr), true
	}
	if currentState != StatePlaying {
		return 0, false
	}
	return playbackPosition().Seconds(), true
}

// tcChaseAdjust tells the pump of owner how far its next frame (take frame
// written) is from where the code wants it: frames to skip (> 0) or to
// insert as silence (< 0), 0 within tolerance or when not chasing.
func tcChaseAdjust(owner any, written int64, ring int64) int64 {
	tcMu.Lock()
	cmd, ref, sr := tcChaseLive.cmd, tcChaseLive.ref, tcChaseLive.sr
	tcMu.Unlock()
	if cmd == nil || cmd != owner || sr == 0 {
		return 0
	}
	r := tcInputNow(time.Now())
	if !r.locked {
		return 0
	}
	target := tcSecondsOfFrames(r.frames, tcRates[r.rate])*float64(sr) - float64(ref)
	want := int64(math.Round(target)) + ring
	diff := want - written
	if tol := int64(tcChaseTolerance.Seconds() * float64(sr)); diff > -tol && diff < tol {
		return 0
	}
	return diff
}

// tcChaseText is the chase state for the UIs ("" when not armed).
func tcChaseTextLocked() string {
	if !tcChase.armed {
		return ""
	}
	return tcChase.state
}
