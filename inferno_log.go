package main

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// inferno2pipe's stderr used to be discarded, so receive-side faults (reorder
// buffer losses, flow timeouts) never reached the journal. Its own log level
// is debug, and a loss is reported once per channel, so lines are counted
// rather than forwarded: the first event is logged at once, then a summary
// at most every infernoLogSummaryEvery.

var infernoLostRe = regexp.MustCompile(`Lost (\d+) samples`)

var (
	infernoLostEvents    atomic.Int64 // "Lost N samples" lines (one per channel per event)
	infernoLostSamples   atomic.Int64 // sum of N
	infernoMediaTimeouts atomic.Int64 // "not receiving media packets"
)

var infernoLogSummaryEvery = 10 * time.Second

func consumeInfernoStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var lastSummary time.Time
	var lostAtSummary, timeoutsAtSummary int64
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "Lost ") && strings.Contains(line, " samples"):
			infernoLostEvents.Add(1)
			if m := infernoLostRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.ParseInt(m[1], 10, 64)
				infernoLostSamples.Add(n)
			}
		case strings.Contains(line, "not receiving media packets"):
			infernoMediaTimeouts.Add(1)
		default:
			continue
		}
		lost, timeouts := infernoLostEvents.Load(), infernoMediaTimeouts.Load()
		if lastSummary.IsZero() || time.Since(lastSummary) >= infernoLogSummaryEvery {
			if lost > lostAtSummary {
				logErrorf("inferno2pipe: %d sample-loss events (%d samples total across channels) - e.g. %s",
					lost-lostAtSummary, infernoLostSamples.Load(), strings.TrimSpace(line))
			}
			if timeouts > timeoutsAtSummary {
				logWarnf("inferno2pipe: %d receive-flow media timeouts", timeouts-timeoutsAtSummary)
			}
			lastSummary, lostAtSummary, timeoutsAtSummary = time.Now(), lost, timeouts
		}
	}
}
