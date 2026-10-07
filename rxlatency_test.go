package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRxLatencyLabelsAndPresets(t *testing.T) {
	for ns, want := range map[int]string{10_000_000: "10 ms", 500_000: "0.5 ms", 2_500_000: "2.5 ms"} {
		if got := rxLatencyLabel(ns); got != want {
			t.Errorf("%d ns = %q, want %q", ns, got, want)
		}
	}
	if rxLatencyPresetIdx(4_000_000) != 2 || rxLatencyPresetIdx(5_000_000) != -1 {
		t.Error("preset index wrong")
	}
	for _, bad := range []int{0, 499_999, 10_000_001} {
		if validRxLatency(bad) {
			t.Errorf("%d accepted", bad)
		}
	}
}

// A controller's latency request is adopted like the unit's own setting,
// and makes the running inferno stale until it restarts with it.
func TestControllerLatencyRequest(t *testing.T) {
	initTestHardware(t)
	mutex.Lock()
	oLat, oLast, oState, oDemo, oPath := rxLatencyNs, lastRxLatencyNs, infernoState, demoMode, infernoLatencyRequestPath
	rate, ch := sampleRates[sampleRateIdx], channelCount
	oRate, oCh := lastSampleRate, lastChannelCount
	rxLatencyNs, lastRxLatencyNs = rxLatencyDefault, rxLatencyDefault
	lastSampleRate, lastChannelCount = rate, ch
	lastRxLatencyNs = rxLatencyNs
	infernoState, demoMode = InfernoRunning, false
	infernoLatencyRequestPath = filepath.Join(t.TempDir(), "req")
	mutex.Unlock()
	quiesceInfernoWorker(t)
	t.Cleanup(func() {
		quiesceInfernoWorker(t)
		mutex.Lock()
		rxLatencyNs, lastRxLatencyNs, infernoState, demoMode, infernoLatencyRequestPath = oLat, oLast, oState, oDemo, oPath
		lastSampleRate, lastChannelCount = oRate, oCh
		mutex.Unlock()
	})

	// Out of range: ignored, and consumed.
	os.WriteFile(infernoLatencyRequestPath, []byte("100\n"), 0o644)
	applyControllerLatency()
	if _, err := os.Stat(infernoLatencyRequestPath); err == nil {
		t.Fatal("request not consumed")
	}
	mutex.Lock()
	if rxLatencyNs != rxLatencyDefault {
		t.Fatalf("an invalid request changed the latency to %d", rxLatencyNs)
	}
	mutex.Unlock()

	// 2.5 ms: not a preset, still taken; inferno now needs a restart.
	os.WriteFile(infernoLatencyRequestPath, []byte("2500000\n"), 0o644)
	applyControllerLatency()
	mutex.Lock()
	got, stale, text := rxLatencyNs, infernoSettingsChangedLocked(), rxLatencyStatusText()
	mutex.Unlock()
	if got != 2_500_000 || !stale || text != "2.5 ms*" {
		t.Fatalf("latency %d, restart needed %v, panel %q", got, stale, text)
	}
	// The panel steps a custom value to the neighbouring presets.
	mutex.Lock()
	adjustRxLatency(1)
	up := rxLatencyNs
	rxLatencyNs = 2_500_000
	adjustRxLatency(-1)
	down := rxLatencyNs
	mutex.Unlock()
	// The list runs 10 ms down to 0.5 ms: forward is the next preset below.
	if up != 2_000_000 || down != 4_000_000 {
		t.Fatalf("from 2.5 ms: forward %d, back %d; want 2 ms and 4 ms", up, down)
	}
}
