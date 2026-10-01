package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Trimmed from a real statime 0.4.0 (inferno-dev) observation of a slave.
const statimeSlaveJSON = `{"program":{"version":"0.4.0"},"instance":{"current_ds":{"steps_removed":1,"offset_from_master":33015000000000,"mean_delay":378923000000000},"port_ds":[{"port_state":"Slave"}]}}`

func serveOnce(t *testing.T, payload string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "obs.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Write([]byte(payload))
		c.Close()
	}()
	return path
}

func TestReadStatimeObservation(t *testing.T) {
	state, offset, err := readStatimeObservation(serveOnce(t, statimeSlaveJSON), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 33015000000000 / 2^32 = 7686.9 ns
	if state != "Slave" || offset != 7687*time.Nanosecond {
		t.Fatalf("got state=%q offset=%v, want Slave 7.687µs", state, offset)
	}
	if _, _, err := readStatimeObservation(filepath.Join(t.TempDir(), "missing.sock"), 100*time.Millisecond); err == nil {
		t.Fatal("missing socket must be an error, not a sync")
	}
}

func TestClockSyncNeedsStableSlaveSamples(t *testing.T) {
	mutex.Lock()
	defer mutex.Unlock()
	saved := clockSync
	defer func() { clockSync = saved }()
	clockSync = clockSyncStatus{}

	now := time.Now()
	for i := 1; i < clockSyncStableSamples; i++ {
		updateClockSyncLocked("Slave", 20*time.Microsecond, nil, now)
		if clockSyncedLocked(now) {
			t.Fatalf("synced after only %d good samples", i)
		}
	}
	updateClockSyncLocked("Slave", 20*time.Microsecond, nil, now)
	if !clockSyncedLocked(now) {
		t.Fatalf("not synced after %d good samples", clockSyncStableSamples)
	}
	if clockSyncedLocked(now.Add(10 * clockSyncPollInterval)) {
		t.Fatal("a stalled poller must not keep reporting sync")
	}
	updateClockSyncLocked("Slave", 2*clockSyncMaxOffset, nil, now)
	if clockSyncedLocked(now) {
		t.Fatal("an offset beyond clockSyncMaxOffset must drop sync")
	}
	for i := 0; i < 2*clockSyncStableSamples; i++ {
		updateClockSyncLocked("Master", 0, nil, now)
	}
	if clockSyncedLocked(now) {
		t.Fatal("a PTP master is not synced to the network")
	}
}

func TestRecordRefusedWithoutClockSync(t *testing.T) {
	initTestHardware(t)
	resetTransportCleanup(t)
	mutex.Lock()
	savedReq, savedSync, savedDemo := clockSyncRequired, clockSync, demoMode
	clockSyncRequired, clockSync, demoMode = true, clockSyncStatus{}, false
	currentState = StateIdle
	updateClockSyncLocked("", 0, os.ErrNotExist, time.Now())
	started := startRecordingGuarded()
	rec, warned := isRecording, time.Now().Before(clockWarnUntil)
	clockSyncRequired, clockSync, demoMode = savedReq, savedSync, savedDemo
	clockWarnUntil = time.Time{}
	mutex.Unlock()
	if started || rec {
		t.Fatal("a take started with no clock sync")
	}
	if !warned {
		t.Fatal("refusal did not raise the NO CLOCK SYNC warning")
	}
}
