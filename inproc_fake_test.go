package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePairedDevice stands in for the inferno plugin's paired device: Read
// delivers silence at real-time pace (like a running instance), Write
// records like fakeTxHolder, and Close ends both. failReadAfter makes Read
// fail once that long has passed since the open, the in-process
// equivalent of a server that dies on its own.
type fakePairedDevice struct {
	fakeTxHolder
	channels, rate int
	opened         time.Time
	failReadAfter  time.Duration
	closeDelay     time.Duration
	noData         bool
	done           chan struct{}
	closeOnce      sync.Once
}

func newFakePairedDevice(rate, channels int) *fakePairedDevice {
	return &fakePairedDevice{rate: rate, channels: channels, opened: time.Now(), done: make(chan struct{})}
}

func (f *fakePairedDevice) Read(buf []int32) (int, error) {
	// 10ms of audio per read, at most what fits.
	frames := min(len(buf)/max(f.channels, 1), max(f.rate/100, 1))
	select {
	case <-f.done:
		return 0, errors.New("fake paired device closed")
	case <-time.After(10 * time.Millisecond):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failReadAfter > 0 && time.Since(f.opened) >= f.failReadAfter {
		return 0, errors.New("fake paired device: capture failed")
	}
	if f.noData {
		return 0, nil
	}
	clear(buf[:frames*f.channels])
	return frames, nil
}

func (f *fakePairedDevice) Close() error {
	f.closeOnce.Do(func() {
		close(f.done)
		time.Sleep(f.closeDelay)
	})
	return f.fakeTxHolder.Close()
}

func (f *fakePairedDevice) Xruns() int64 { return 0 }

// fakeInferno is the installed openPairedDevice seam: it records every
// device it opened, and openErr makes opens fail (no plugin).
type fakeInferno struct {
	mu            sync.Mutex
	devices       []*fakePairedDevice
	openErr       error
	writeErr      error         // every device's Writes fail (no clock overlay)
	closeDelay    time.Duration // every device's Close takes this long
	noData        bool          // Reads deliver no frames (no media yet)
	failReadAfter time.Duration
}

func (fi *fakeInferno) open(rate, channels int) (pairedDevice, error) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.openErr != nil {
		return nil, fi.openErr
	}
	d := newFakePairedDevice(rate, channels)
	d.failReadAfter = fi.failReadAfter
	d.writeErr = fi.writeErr
	d.closeDelay = fi.closeDelay
	d.noData = fi.noData
	fi.devices = append(fi.devices, d)
	return d, nil
}

func (fi *fakeInferno) opens() int {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return len(fi.devices)
}

func (fi *fakeInferno) last() *fakePairedDevice {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if len(fi.devices) == 0 {
		return nil
	}
	return fi.devices[len(fi.devices)-1]
}

// useFakeInferno installs a fake inferno plugin for the test: starts open
// a fakePairedDevice instead of failing like the suite default. The seam
// is swapped through the inferno worker's queue so no start in flight
// sees it change halfway.
func useFakeInferno(t *testing.T) *fakeInferno {
	t.Helper()
	fi := &fakeInferno{}
	quiesceInfernoWorker(t)
	orig := openPairedDevice
	openPairedDevice = fi.open
	t.Cleanup(func() {
		quiesceInfernoWorker(t)
		openPairedDevice = orig
	})
	return fi
}

// waitFor polls cond until it holds or within passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The capture loop's FIFO descriptor must not leak into children: an
// inherited write end kept every later ffmpeg from seeing EOF when Inferno
// stopped, so each one sat out the stop grace and was SIGKILLed.
func TestInfernoFifoNotInheritedByChildren(t *testing.T) {
	initTestHardware(t)
	saveTxGlobals(t)
	useFakeInferno(t)
	mutex.Lock()
	demoMode = false
	mutex.Unlock()
	t.Cleanup(doStopInferno)
	doStartInferno()
	mutex.Lock()
	path := fifoPath
	mutex.Unlock()
	if path == "" {
		t.Fatal("setup: Inferno did not start")
	}
	time.Sleep(50 * time.Millisecond) // the capture loop opens the FIFO
	out, err := exec.Command("sh", "-c", `for f in /proc/$$/fd/*; do readlink "$f"; done; true`).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), filepath.Base(path)) {
		t.Fatalf("a child inherited the FIFO:\n%s", out)
	}
}

// The meters lagged the audio by seconds: ffmpeg buffered the metadata
// lines, and the reader published only every 32 values. The filter must
// print unbuffered, and each window must reach the meters as soon as the
// next one starts - before the stream ends.
func TestMeterWindowsPublishImmediately(t *testing.T) {
	if !strings.Contains(meterFilterChain(48000), "ametadata=print:file=-:direct=1") {
		t.Fatalf("meter filter prints buffered: %s", meterFilterChain(48000))
	}
	mutex.Lock()
	gen := meterGen
	origPeak, origCh := meterPeakDB, meterChannelPeak
	meterChannelPeak = []float64{-100, -100}
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		meterPeakDB, meterChannelPeak = origPeak, origCh
		mutex.Unlock()
	})
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { meterReader(pr, gen); close(done) }()
	fmt.Fprint(pw, "frame:0    pts:0       pts_time:0\n"+
		"lavfi.astats.1.Peak_level=-12.5\n"+
		"lavfi.astats.2.Peak_level=-14.0\n"+
		"lavfi.astats.Overall.Peak_level=-12.5\n"+
		"frame:1    pts:4800    pts_time:0.1\n")
	waitFor(t, time.Second, "the first window on the meters", func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		return meterPeakDB == -12.5 && meterChannelPeak[1] == -14.0
	})
	pw.Close()
	<-done
}
