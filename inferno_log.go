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
	var windowStart time.Time
	forwarded, suppressed := 0, 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case infernoFaultLine(line):
			// Anything else the server reports at ERROR, and a Rust
			// panic, used to be dropped with the debug chatter - the one
			// line saying why it died never reached the journal. Rate
			// limited per summary window so a fault loop can't flood it.
			if now := time.Now(); now.Sub(windowStart) >= infernoLogSummaryEvery {
				if suppressed > 0 {
					logErrorf("inferno2pipe: %d more error lines suppressed", suppressed)
				}
				windowStart, forwarded, suppressed = now, 0, 0
			}
			if forwarded < infernoFaultLinesPerWindow {
				forwarded++
				logErrorf("inferno2pipe: %s", strings.TrimSpace(line))
			} else {
				suppressed++
			}
			continue
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
	if err := sc.Err(); err != nil {
		// The scanner gives up on an over-long line. Stopping here left
		// the pipe unread: once its buffer filled, the server blocked on
		// its next stderr write and the audio stalled with it. Keep
		// draining, unparsed, until the server closes stderr.
		logWarnf("inferno2pipe stderr: %v - draining the rest unparsed", err)
		io.Copy(io.Discard, r)
	}
}

// infernoFaultLinesPerWindow caps forwarded server error lines per
// infernoLogSummaryEvery.
const infernoFaultLinesPerWindow = 5

// infernoFaultLine reports a server line worth forwarding verbatim: an
// ERROR-level record (the counted sample-loss lines are matched before this
// is consulted) or a Rust panic.
func infernoFaultLine(line string) bool {
	if strings.Contains(line, "Lost ") && strings.Contains(line, " samples") {
		return false
	}
	return strings.Contains(line, " ERROR ") || strings.Contains(line, "panicked at")
}
