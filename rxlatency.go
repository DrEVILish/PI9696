package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// The inferno receive latency.
//
// How far behind the network's media clock the unit plays out what it
// receives: inferno's RX_LATENCY_NS (raised per flow to what a sender asks
// for). Lower is less delay through the unit, and less timecode offset to
// compensate; too low for the network and received audio drops out.
//
// The WebUI and the panel offer rxLatencyPresets. A network controller
// (netaudio, Dante Controller) may set any value within the range the
// device announces (fork 5981fee: ARC 0x1101 is handed over through
// INFERNO_LATENCY_REQUEST_PATH): the app adopts it like its own setting -
// persisted and applied by restarting inferno, deferred while a take
// records or plays. A value that is not a preset shows as custom.

// rxLatencyPresets are the offered receive latencies in ns.
var rxLatencyPresets = []int{10_000_000, 6_000_000, 4_000_000, 2_000_000, 1_000_000, 500_000}

// The range a controller may set (as the fork announces it).
const (
	rxLatencyMin     = 500_000
	rxLatencyMax     = 10_000_000
	rxLatencyDefault = 10_000_000
)

var (
	rxLatencyNs     = rxLatencyDefault // the setting (app mutex)
	lastRxLatencyNs int                // what the running inferno was started with (app mutex)
)

// rxLatencyLive is the running instance's latency for the audio threads
// (the timecode engine's receive-latency compensation).
var rxLatencyLive atomic.Int64

func init() { rxLatencyLive.Store(rxLatencyDefault) }

// infernoLatencyRequestPath is where the fork writes a controller's latency
// change. A var so tests can point it elsewhere.
var infernoLatencyRequestPath = "/var/lib/pi9696/inferno-latency-request"

func validRxLatency(ns int) bool { return ns >= rxLatencyMin && ns <= rxLatencyMax }

// rxLatencyLabel shows a latency in ms: "10 ms", "0.5 ms".
func rxLatencyLabel(ns int) string {
	return strconv.FormatFloat(float64(ns)/1e6, 'f', -1, 64) + " ms"
}

// rxLatencyPresetIdx is the setting's preset index, -1 for a custom value.
func rxLatencyPresetIdx(ns int) int {
	for i, p := range rxLatencyPresets {
		if p == ns {
			return i
		}
	}
	return -1
}

// setRxLatencyLocked changes the setting and restarts inferno to apply it
// (deferred while busy). Caller holds the app mutex.
func setRxLatencyLocked(ns int, by string) {
	if !validRxLatency(ns) || ns == rxLatencyNs {
		return
	}
	logInfof("Receive latency set to %s %s", rxLatencyLabel(ns), by)
	rxLatencyNs = ns
	settingChanged()
	checkInfernoRestart()
}

// adjustRxLatency steps the panel's RX Latency row through the presets
// (a custom value steps to the nearest preset). Caller holds the app mutex.
func adjustRxLatency(direction int) {
	i := rxLatencyPresetIdx(rxLatencyNs)
	if i < 0 {
		// Custom: the first preset below it going down, above it going up.
		i = len(rxLatencyPresets) - 1
		for j, p := range rxLatencyPresets {
			if p < rxLatencyNs {
				i = j
				break
			}
		}
		if direction < 0 && i > 0 {
			i--
		}
		direction = 0
	}
	n := len(rxLatencyPresets)
	setRxLatencyLocked(rxLatencyPresets[((i+direction)%n+n)%n], "from the front panel")
}

// applyControllerLatency consumes a pending latency request from a network
// controller, if any.
func applyControllerLatency() {
	data, err := os.ReadFile(infernoLatencyRequestPath)
	if err != nil {
		return
	}
	if err := os.Remove(infernoLatencyRequestPath); err != nil {
		logWarnf("controller latency: cannot remove %s: %v", infernoLatencyRequestPath, err)
	}
	ns, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !validRxLatency(ns) {
		logWarnf("controller latency: ignoring request %q", strings.TrimSpace(string(data)))
		return
	}
	mutex.Lock()
	setRxLatencyLocked(ns, "by a network controller")
	mutex.Unlock()
}

// rxLatencyStatusText is the panel row's value.
func rxLatencyStatusText() string {
	s := rxLatencyLabel(rxLatencyNs)
	if rxLatencyPresetIdx(rxLatencyNs) < 0 {
		s += "*"
	}
	return s
}

func rxLatencyEnv(ns int) string { return fmt.Sprintf("%d", ns) }
