package main

import (
	"strings"
	"testing"
)

func TestConsumeInfernoStderrCounts(t *testing.T) {
	l0, s0, t0 := infernoLostEvents.Load(), infernoLostSamples.Load(), infernoMediaTimeouts.Load()
	in := strings.Join([]string{
		"[2026-09-30T22:03:57Z ERROR inferno_aoip::device_server::samples_collector] Lost 65536 samples at timestamp 16449304712 in channel id 1 (reorder buffer timeout)",
		"[2026-09-30T22:03:57Z ERROR inferno_aoip::device_server::samples_collector] Lost 2937 samples at timestamp 16449304712 in channel id 2 (reorder buffer timeout)",
		"[2026-09-30T22:38:42Z WARN  inferno_aoip::device_server::channels_subscriber] flow index=0 timeout (not receiving media packets)",
		"[2026-09-30T22:38:42Z DEBUG inferno_aoip::device_server::flows_rx] adding socket",
	}, "\n")
	consumeInfernoStderr(strings.NewReader(in))
	if got := infernoLostEvents.Load() - l0; got != 2 {
		t.Errorf("loss events = %d, want 2", got)
	}
	if got := infernoLostSamples.Load() - s0; got != 65536+2937 {
		t.Errorf("lost samples = %d, want %d", got, 65536+2937)
	}
	if got := infernoMediaTimeouts.Load() - t0; got != 1 {
		t.Errorf("media timeouts = %d, want 1", got)
	}
}
