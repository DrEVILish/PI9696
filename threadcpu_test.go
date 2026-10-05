package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func statLine(pid int, comm string, utime, stime int64) string {
	return fmt.Sprintf("%d (%s) S 1 1 1 0 -1 4194560 100 0 0 0 %d %d 0 0 20 0 1 0 1 0 0\n", pid, comm, utime, stime)
}

func TestProcCPUTicks(t *testing.T) {
	comm, ticks, ok := procCPUTicks(statLine(7, "flows RX", 120, 30))
	if !ok || comm != "flows RX" || ticks != 150 {
		t.Fatalf("got %q %d %v, want \"flows RX\" 150 true", comm, ticks, ok)
	}
	// A comm containing ") " must not shift the fields.
	comm, ticks, ok = procCPUTicks(statLine(8, "a) b (c", 5, 6))
	if !ok || comm != "a) b (c" || ticks != 11 {
		t.Fatalf("got %q %d %v for a tricky comm", comm, ticks, ok)
	}
	for _, bad := range []string{"", "12 no-parens S 1", "1 (x) S 1 2 3"} {
		if _, _, ok := procCPUTicks(bad); ok {
			t.Errorf("parsed malformed stat %q", bad)
		}
	}
}

func TestSubsystemOf(t *testing.T) {
	for comm, want := range map[string]string{
		threadRender: "OLED render", threadRxCapture: "Capture", threadTxPump: "Playback", threadTxIdle: "Playback",
		"flows RX": "Inferno media", "flows TX": "Inferno media", "Inferno main": "Inferno control", "pi9696": "Other",
	} {
		if got := subsystemOf(comm, false); got != want {
			t.Errorf("thread %q -> %q, want %q", comm, got, want)
		}
	}
	if got := subsystemOf("ffmpeg", true); got != "ffmpeg" {
		t.Errorf("child ffmpeg -> %q", got)
	}
	if got := subsystemOf("pi-render", true); got != "Other" {
		t.Errorf("a child process is never an app thread, got %q", got)
	}
	// Every subsystemOf result must be a telemetry column.
	seen := map[string]bool{}
	for _, n := range cpuSubsystems {
		seen[n] = true
	}
	for _, c := range []string{threadRender, threadRxCapture, threadTxPump, "flows RX", "Inferno main", "x"} {
		if !seen[subsystemOf(c, false)] {
			t.Errorf("%q maps outside cpuSubsystems", c)
		}
	}
}

// A fake /proc: threads of "self" plus one ffmpeg child, sampled twice
// two seconds apart.
func TestCPUSubsysSampler(t *testing.T) {
	root := t.TempDir()
	write := func(rel, data string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setTicks := func(render, flows, other, ffmpeg int64) {
		write("self/task/10/stat", statLine(10, "pi9696", other, 0))
		write("self/task/10/children", "500 ")
		write("self/task/11/stat", statLine(11, threadRender, render, 0))
		write("self/task/11/children", "")
		write("self/task/12/stat", statLine(12, "flows RX", flows/2, flows-flows/2))
		write("500/stat", statLine(500, "ffmpeg", ffmpeg, 0))
	}
	var s cpuSubsysSampler
	t0 := time.Unix(1000, 0)
	setTicks(100, 1000, 50, 7)
	if got := s.sample(root, t0); got != nil {
		t.Fatalf("first sample should only prime, got %v", got)
	}
	// +20 ticks in 2s = 10% of a core, +60 = 30%, +4 = 2%, +8 = 4%.
	setTicks(120, 1060, 54, 15)
	got := s.sample(root, t0.Add(2*time.Second))
	want := map[string]float64{"OLED render": 10, "Inferno media": 30, "Other": 2, "ffmpeg": 4}
	for i, name := range cpuSubsystems {
		if d := got[i] - want[name]; d > 1e-9 || d < -1e-9 {
			t.Errorf("%s = %v, want %v", name, got[i], want[name])
		}
	}
	// A thread that appears counts all its time as new; one that is gone
	// contributes nothing (and never a negative).
	os.RemoveAll(filepath.Join(root, "self/task/12"))
	write("self/task/13/stat", statLine(13, threadRxCapture, 6, 0))
	got = s.sample(root, t0.Add(4*time.Second))
	for i, name := range cpuSubsystems {
		w := 0.0
		if name == "Capture" {
			w = 3
		}
		if d := got[i] - w; d > 1e-9 || d < -1e-9 {
			t.Errorf("after churn %s = %v, want %v", name, got[i], w)
		}
	}
}

func TestNameThread(t *testing.T) {
	done := make(chan string)
	go func() {
		nameThread("pi-test-a-very-long-name")
		data, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/comm", syscall.Gettid()))
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- strings.TrimSpace(string(data))
	}()
	if got := <-done; got != "pi-test-a-very-" {
		t.Fatalf("thread comm = %q, want the first 15 bytes", got)
	}
}
