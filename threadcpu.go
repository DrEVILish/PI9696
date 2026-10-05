package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Per-subsystem CPU metering.
//
// Go multiplexes goroutines over unnamed threads, so a process-wide CPU
// figure can't say whether the OLED, the audio path or the WebUI is
// costing it. The realtime goroutines therefore pin themselves to a
// named OS thread (nameThread), the in-process inferno plugin already
// names its own threads, and cpuSubsysSampler groups /proc thread and
// child-process CPU time by those names. The result is in the telemetry
// history and the WebUI's "CPU by subsystem" graph, and visible in top -H.
// Everything else (WebUI handlers, HyperDeck, the Go runtime) is "other";
// pprof labels (PI9696_PPROF) split that further when needed.

const (
	threadRender    = "pi-render"
	threadRxCapture = "pi-rx-capture"
	threadTxPump    = "pi-tx-pump"
	threadTxIdle    = "pi-tx-idle"
)

// cpuSubsystems is the fixed order of telemetry columns.
var cpuSubsystems = []string{"OLED render", "Capture", "Playback", "Inferno media", "Inferno control", "ffmpeg", "Other"}

// subsystemOf maps a thread or child-process name to its cpuSubsystems
// entry.
func subsystemOf(comm string, child bool) string {
	switch {
	case child && comm == "ffmpeg":
		return "ffmpeg"
	case child:
		return "Other"
	case comm == threadRender:
		return "OLED render"
	case comm == threadRxCapture:
		return "Capture"
	case comm == threadTxPump, comm == threadTxIdle:
		return "Playback"
	case strings.HasPrefix(comm, "flows "): // the plugin's "flows RX"/"flows TX"
		return "Inferno media"
	case strings.HasPrefix(comm, "Inferno"), strings.HasPrefix(comm, "tokio"):
		return "Inferno control"
	}
	return "Other"
}

// nameThread locks the calling goroutine to its OS thread for the rest of
// its life and names the thread (15 bytes max). Only for long-lived
// goroutines: one that exits still locked takes its thread with it, so a
// renamed thread is never handed to other goroutines.
func nameThread(name string) {
	runtime.LockOSThread()
	b := append([]byte(name), 0)
	if len(b) > 16 {
		b = append(b[:15], 0)
	}
	const prSetName = 15
	syscall.RawSyscall(syscall.SYS_PRCTL, prSetName, uintptr(unsafe.Pointer(&b[0])), 0)
	runtime.KeepAlive(b)
}

// clkTck is USER_HZ, the unit of /proc .../stat utime and stime: 100 on
// every Linux the unit runs.
const clkTck = 100

// procCPUTicks parses the comm and utime+stime of a /proc/<pid>/stat or
// task stat line. comm is in parentheses and may contain spaces.
func procCPUTicks(stat string) (comm string, ticks int64, ok bool) {
	open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || close < open {
		return "", 0, false
	}
	f := strings.Fields(stat[close+1:])
	// After the comm: state ppid pgrp session tty tpgid flags minflt
	// cminflt majflt cmajflt utime stime -> utime is f[11], stime f[12].
	if len(f) < 13 {
		return "", 0, false
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	s, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return "", 0, false
	}
	return stat[open+1 : close], u + s, true
}

type cpuSubsysSampler struct {
	prev map[string]int64 // "t<tid>" / "c<pid>" -> ticks at the last sample
	at   time.Time
}

// sample returns each subsystem's CPU since the previous call, in percent
// of one core, in cpuSubsystems order. The first call only primes the
// sampler and returns nil.
func (s *cpuSubsysSampler) sample(procRoot string, now time.Time) []float64 {
	cur := map[string]int64{}
	group := map[string]int64{}
	add := func(key, sub string, ticks int64) {
		cur[key] = ticks
		if prev, ok := s.prev[key]; ok && ticks >= prev {
			group[sub] += ticks - prev
		} else if !ok && s.prev != nil {
			// Born since the last sample: all of its time is new.
			group[sub] += ticks
		}
	}
	self := filepath.Join(procRoot, "self")
	tasks, _ := os.ReadDir(filepath.Join(self, "task"))
	for _, t := range tasks {
		data, err := os.ReadFile(filepath.Join(self, "task", t.Name(), "stat"))
		if err != nil {
			continue
		}
		if comm, ticks, ok := procCPUTicks(string(data)); ok {
			add("t"+t.Name(), subsystemOf(comm, false), ticks)
		}
		kids, _ := os.ReadFile(filepath.Join(self, "task", t.Name(), "children"))
		for _, pid := range strings.Fields(string(kids)) {
			data, err := os.ReadFile(filepath.Join(procRoot, pid, "stat"))
			if err != nil {
				continue
			}
			if comm, ticks, ok := procCPUTicks(string(data)); ok {
				add("c"+pid, subsystemOf(comm, true), ticks)
			}
		}
	}
	prevAt := s.at
	primed := s.prev != nil
	s.prev, s.at = cur, now
	if !primed {
		return nil
	}
	secs := now.Sub(prevAt).Seconds()
	out := make([]float64, len(cpuSubsystems))
	if secs <= 0 {
		return out
	}
	for i, name := range cpuSubsystems {
		out[i] = float64(group[name]) / clkTck / secs * 100
	}
	return out
}
