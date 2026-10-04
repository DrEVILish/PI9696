package alsapcm

import (
	"testing"
	"time"
)

// The dev box has no Inferno device, so the wrapper is exercised for its
// argument handling and its behaviour when the device is absent - the two
// places a silent mistake would otherwise only show up on the unit.
func TestOpenRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct{ rate, channels int }{
		{0, 2}, {48000, 0}, {-1, 2}, {48000, -1},
	} {
		if _, err := Open("inferno", tc.rate, tc.channels); err == nil {
			t.Fatalf("Open(rate=%d, channels=%d) accepted invalid config", tc.rate, tc.channels)
		}
	}
}

// A missing device must surface as a named error rather than a bare integer,
// because this is the message an operator sees when inferno's ALSA module or
// its device config is wrong.
func TestOpenMissingDeviceIsNamed(t *testing.T) {
	_, err := Open("no-such-pcm-xyzzy", 48000, 2)
	if err == nil {
		t.Fatal("Open on a nonexistent device returned no error")
	}
	if got := err.Error(); got == "" || !contains(got, "no such ALSA device") {
		t.Fatalf("error = %q, want it to name the missing device", got)
	}
}

// Reading or writing a closed device is a programming error, not something to
// paper over: the caller rebuilds the device when audio settings change.
func TestUseAfterClose(t *testing.T) {
	d := &Device{closed: true, channels: 2}
	if _, err := d.Read(make([]int32, 64)); err == nil {
		t.Error("Read on a closed device returned no error")
	}
	if _, err := d.Write(make([]int32, 64)); err == nil {
		t.Error("Write on a closed device returned no error")
	}
	// Close must be idempotent: it doubles as a finalizer.
	if err := d.Close(); err != nil {
		t.Errorf("Close on an already-closed device: %v", err)
	}
	var nilDev *Device
	if err := nilDev.Close(); err != nil {
		t.Errorf("Close on nil device: %v", err)
	}
}

// A buffer holding less than one whole frame must be refused rather than
// partially consumed: the app sizes buffers by channel count, and reading a
// fraction of a frame would shift every later sample. The dev box has no
// Inferno device, so this covers only the pre-flight checks; the frame
// clamping itself is exercised on the unit.
func TestSubFrameBufferIsIgnored(t *testing.T) {
	// 2 channels, so 3 samples is one whole frame plus a stray sample.
	if n, err := (&Device{channels: 2, framesPerIO: 1024}).Read(make([]int32, 3)); err == nil {
		t.Errorf("Read with no open handle returned (%d, nil), want an error", n)
	}
	// 1 sample at 2 channels is not a whole frame: must be a no-op, and must
	// not reach ALSA (which would abort on a nil handle).
	d := &Device{channels: 2, framesPerIO: 1024, cap: nil}
	if n, err := d.Read(make([]int32, 1)); n != 0 || err != nil {
		t.Errorf("Read of a sub-frame buffer = (%d, %v), want (0, nil)", n, err)
	}
	d2 := &Device{channels: 2, framesPerIO: 1024, play: nil}
	if n, err := d2.Write(make([]int32, 1)); n != 0 || err != nil {
		t.Errorf("Write of a sub-frame buffer = (%d, %v), want (0, nil)", n, err)
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A zero-value Device must not panic: Read/Write divide by the channel count,
// and a panic on the audio path would take the recorder down.
func TestZeroValueDeviceDoesNotPanic(t *testing.T) {
	var d Device
	if _, err := d.Read(make([]int32, 64)); err == nil {
		t.Error("Read on a zero-value device returned no error")
	}
	if _, err := d.Write(make([]int32, 64)); err == nil {
		t.Error("Write on a zero-value device returned no error")
	}
}

// OpenPlayback mirrors Open's validation and its missing-device behaviour: a
// TX-only holder must fail the same way, not a new way, so the app's
// device-absent fallback treats both identically.
func TestOpenPlaybackRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct{ rate, channels int }{
		{0, 2}, {48000, 0}, {-1, 2}, {48000, -1},
	} {
		if _, err := OpenPlayback("inferno", tc.rate, tc.channels); err == nil {
			t.Fatalf("OpenPlayback(rate=%d, channels=%d) accepted invalid config", tc.rate, tc.channels)
		}
	}
}

func TestOpenPlaybackMissingDeviceIsNamed(t *testing.T) {
	_, err := OpenPlayback("no-such-pcm-xyzzy", 48000, 2)
	if err == nil {
		t.Fatal("OpenPlayback on a nonexistent device returned no error")
	}
	if got := err.Error(); got == "" || !contains(got, "no such ALSA device") {
		t.Fatalf("error = %q, want it to name the missing device", got)
	}
}

// A playback-only device must refuse capture: the TX holder never reads, and
// a Read slipping through would prepare a capture stream the instance never
// asked for.
func TestPlaybackOnlyRefusesRead(t *testing.T) {
	d := &Device{channels: 2, framesPerIO: 1024}
	if _, err := d.Read(make([]int32, 64)); err == nil {
		t.Error("Read on a playback-only device returned no error")
	}
}

// Close used to free the handles while a Read or Write on another goroutine
// could still be inside ALSA (a use-after-free in C). It must wait for the
// in-flight call; holding the direction's lock stands in for one.
func TestCloseWaitsForInFlightIO(t *testing.T) {
	for _, dir := range []string{"read", "write"} {
		d := &Device{channels: 2, framesPerIO: 1024}
		mu := &d.capMu
		if dir == "write" {
			mu = &d.playMu
		}
		mu.Lock() // an I/O call in progress
		closed := make(chan struct{})
		go func() {
			d.Close()
			close(closed)
		}()
		select {
		case <-closed:
			t.Fatalf("Close returned during an in-flight %s", dir)
		case <-time.After(50 * time.Millisecond):
		}
		mu.Unlock() // the call returns
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("Close never completed after the %s returned", dir)
		}
		if _, err := d.Read(make([]int32, 4)); err == nil {
			t.Fatal("Read after Close succeeded")
		}
	}
}
