package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"time"
)

// Recording is only allowed while the inferno clock is locked to a PTP leader
// on the network. The authority is statime's observation socket: the
// usrvclock overlay alone cannot tell a network lock from the single-host
// stub, which always reports "valid".

var (
	statimeObservationPath = statimeObservationPathFromEnv()
	clockSyncMaxOffset     = time.Millisecond
	clockSyncStableSamples = 5
	clockSyncPollInterval  = time.Second
	// clockSyncRequired is a test seam only: TestMain clears it so the
	// transport tests are not tied to a statime instance; the gate tests
	// set it back.
	clockSyncRequired = true
)

func statimeObservationPathFromEnv() string {
	if p := os.Getenv("PI9696_STATIME_OBS"); p != "" {
		return p
	}
	return "/run/statime/observation.sock"
}

// clockSyncStatus is guarded by the app mutex.
type clockSyncStatus struct {
	portState  string // statime port state, or "" when statime is unreachable
	offset     time.Duration
	goodStreak int
	lastErr    string
	checked    time.Time
}

var clockSync clockSyncStatus

type statimeObservation struct {
	Instance struct {
		CurrentDS struct {
			OffsetFromMaster float64 `json:"offset_from_master"`
		} `json:"current_ds"`
		PortDS []struct {
			PortState string `json:"port_state"`
		} `json:"port_ds"`
	} `json:"instance"`
}

// readStatimeObservation returns the first port's state and the offset from
// the master. statime serialises Duration as nanoseconds in I96F32 fixed
// point (to_bits), i.e. nanoseconds * 2^32.
func readStatimeObservation(path string, timeout time.Duration) (string, time.Duration, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	raw, err := io.ReadAll(io.LimitReader(conn, 1<<20))
	if err != nil {
		return "", 0, err
	}
	var obs statimeObservation
	if err := json.Unmarshal(raw, &obs); err != nil {
		return "", 0, fmt.Errorf("statime observation: %w", err)
	}
	if len(obs.Instance.PortDS) == 0 {
		return "", 0, fmt.Errorf("statime observation: no ports")
	}
	ns := obs.Instance.CurrentDS.OffsetFromMaster / (1 << 32)
	return obs.Instance.PortDS[0].PortState, time.Duration(math.Round(ns)), nil
}

// sampleIsGood: a slave port within clockSyncMaxOffset of its master. A
// master port is never "synced to the network" - it is the network's clock,
// and on a LAN with no other PTP device that means nobody is checking it.
func sampleIsGood(state string, offset time.Duration) bool {
	return state == "Slave" && offset.Abs() <= clockSyncMaxOffset
}

// updateClockSyncLocked folds one poll result into clockSync. Callers hold
// the app mutex.
func updateClockSyncLocked(state string, offset time.Duration, err error, now time.Time) {
	was := clockSyncedLocked(now)
	clockSync.checked = now
	if err != nil {
		clockSync.portState, clockSync.offset, clockSync.goodStreak = "", 0, 0
		clockSync.lastErr = err.Error()
	} else {
		clockSync.portState, clockSync.offset, clockSync.lastErr = state, offset, ""
		if sampleIsGood(state, offset) {
			clockSync.goodStreak++
		} else {
			clockSync.goodStreak = 0
		}
	}
	if is := clockSyncedLocked(now); is != was {
		if is {
			logInfof("Clock synced to the network (PTP %s, offset %v)", state, offset)
		} else if isRecording {
			logWarnf("Clock sync lost during a take: %s", clockSyncTextLocked(now))
		} else {
			logWarnf("Clock not synced: %s", clockSyncTextLocked(now))
		}
	}
}

// clockSyncedLocked: enough consecutive good samples, and the poller is alive.
func clockSyncedLocked(now time.Time) bool {
	if clockSync.checked.IsZero() || now.Sub(clockSync.checked) > 3*clockSyncPollInterval {
		return false
	}
	return clockSync.goodStreak >= clockSyncStableSamples
}

// recordingClockOKLocked is the record gate's view of clock sync.
func recordingClockOKLocked() bool {
	return !clockSyncRequired || demoMode || clockSyncedLocked(time.Now())
}

// clockSyncTextLocked is the operator-facing state for the dashboard/logs.
func clockSyncTextLocked(now time.Time) string {
	switch {
	case clockSync.checked.IsZero():
		return "Checking"
	case clockSync.portState == "":
		return "Not synced (statime unreachable)"
	case clockSyncedLocked(now):
		return fmt.Sprintf("Synced (PTP slave, %v)", clockSync.offset.Round(time.Microsecond))
	case clockSync.portState == "Slave":
		return fmt.Sprintf("Locking (PTP slave, %v)", clockSync.offset.Round(time.Microsecond))
	default:
		return fmt.Sprintf("Not synced (PTP %s)", clockSync.portState)
	}
}

func clockSyncLoop(stop <-chan struct{}) {
	t := time.NewTicker(clockSyncPollInterval)
	defer t.Stop()
	for {
		state, offset, err := readStatimeObservation(statimeObservationPath, clockSyncPollInterval/2)
		mutex.Lock()
		updateClockSyncLocked(state, offset, err, time.Now())
		mutex.Unlock()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}
