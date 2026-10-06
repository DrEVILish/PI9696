package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A child that ignores SIGTERM (as ffmpeg blocked on a full pipe does) is
// killed once the grace runs out, so Stop on a paused take always ends it.
func TestKillIfStillRunningEndsAStuckChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", `trap "" TERM; sleep 30`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the trap install
	cmd.Process.Signal(syscall.SIGTERM)
	killIfStillRunning(cmd.Process, 200*time.Millisecond, "test")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("child ended with %v, want SIGKILL", cmd.ProcessState)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("a child ignoring SIGTERM was not killed")
	}
}

// A child already reaped is left alone.
func TestKillIfStillRunningSkipsAReapedChild(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	killIfStillRunning(cmd.Process, 10*time.Millisecond, "test")
	time.Sleep(50 * time.Millisecond) // the timer fires and must not panic or signal
}
