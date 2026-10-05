package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pi9696/hardware"
)

// Owner rules for the panel transport: only the Play key pauses and
// resumes; the encoder push does neither; the encoder scrubs only while
// paused; Stop ends playback and the next Play starts from the beginning.
func TestTransportKeyRoles(t *testing.T) {
	initTestHardware(t)
	fakeExecutable(t, "ffmpeg", fakeChildScript)
	os.MkdirAll(RecordPath, 0755)
	rec := filepath.Join(RecordPath, "recording_20260101_000000_ch2_48kHz.wav")
	if err := os.WriteFile(rec, make([]byte, 44+60*48000*2*3), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(rec) })
	mutex.Lock()
	currentState, isRecording = StateIdle, false
	sampleRateIdx, channelCount = 1, 2
	mutex.Unlock()

	state := func() AppState {
		mutex.Lock()
		defer mutex.Unlock()
		return currentState
	}
	pos := func() time.Duration {
		mutex.Lock()
		defer mutex.Unlock()
		return playbackPosition()
	}

	onButtonPress(hardware.PlayButton)
	if state() != StatePlaying {
		t.Fatalf("Play from idle: state %v", state())
	}
	onEncoderClick()
	if state() != StatePlaying {
		t.Fatalf("encoder push while playing changed the transport to %v", state())
	}
	onEncoderRotate(1)
	if p := pos(); p > 2*time.Second {
		t.Fatalf("encoder turn while playing scrubbed to %v", p)
	}

	onButtonPress(hardware.PlayButton)
	if state() != StatePaused {
		t.Fatalf("Play while playing should pause, got %v", state())
	}
	onEncoderClick()
	if state() != StatePaused {
		t.Fatalf("encoder push while paused changed the transport to %v", state())
	}
	onEncoderRotate(1)
	if p := pos(); p < 5*time.Second {
		t.Fatalf("encoder turn while paused should scrub ~5s, at %v", p)
	}

	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
	onButtonPress(hardware.PlayButton)
	if state() != StatePlaying {
		t.Fatalf("Play after Stop: state %v", state())
	}
	if p := pos(); p > 2*time.Second {
		t.Fatalf("Play after Stop resumed at %v, want the beginning", p)
	}
	onButtonPress(hardware.StopButton)
	waitForPlaybackIdle(t)
}
