package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
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

// ERROR records and panics other than the counted loss lines must reach the
// log (they were dropped with the debug chatter), rate limited.
func TestConsumeInfernoStderrForwardsFaults(t *testing.T) {
	var buf syncBuffer
	captureLogs(t, &buf)
	lines := []string{
		"[2026-09-30T22:03:57Z DEBUG inferno_aoip::flows_rx] adding socket",
		"[2026-09-30T22:03:57Z ERROR inferno_aoip::device_server] failed to bind 4440: Address in use",
		"thread 'main' panicked at src/main.rs:42:5:",
	}
	for i := 0; i < 20; i++ {
		lines = append(lines, "[2026-09-30T22:03:58Z ERROR inferno_aoip::x] repeated fault")
	}
	consumeInfernoStderr(strings.NewReader(strings.Join(lines, "\n")))
	out := buf.String()
	for _, want := range []string{"Address in use", "panicked at"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "adding socket") {
		t.Error("debug chatter forwarded")
	}
	if n := strings.Count(out, "repeated fault"); n > infernoFaultLinesPerWindow {
		t.Errorf("%d repeated lines forwarded, cap %d", n, infernoFaultLinesPerWindow)
	}
}

// An over-long line made the scanner give up, after which nobody read the
// pipe and the server blocked on stderr. The consumer must keep draining.
func TestConsumeInfernoStderrDrainsAfterLongLine(t *testing.T) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		consumeInfernoStderr(pr)
		close(done)
	}()
	wrote := make(chan error, 1)
	go func() {
		long := bytes.Repeat([]byte("x"), 2<<20) // past the 1 MiB line cap
		if _, err := pw.Write(long); err != nil {
			wrote <- err
			return
		}
		_, err := pw.Write(bytes.Repeat([]byte("more output\n"), 100000))
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer blocked: stderr stopped being drained after a long line")
	}
	pw.Close()
	<-done
}
