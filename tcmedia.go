package main

import (
	"time"

	"pi9696/alsapcm"
)

// The media clock: timecode timing in samples, not wall time.
//
// The inferno plugin moves both streams' hardware pointers by the network's
// PTP media clock (alsapcm.ClockPoint), so a capture-stream position and a
// transmit-stream position that belong to the same media instant differ by
// a constant: the two streams' start times. That constant comes from the
// two clock points (each taken right after a read or write, with
// snd_pcm_delay), and with it the chase and the LTC relay place every
// transmitted sample against the incoming code to the sample - instead of
// estimating "when will this write be heard" from the wall clock, which left
// tens of milliseconds of error.
//
// Positions on the capture side are in the capture loop's own count (the
// stream position the LTC reader and the take stamps use).

// streamClocks is what the paired device reports (alsapcm.Device).
type streamClocks interface {
	CaptureClock() alsapcm.ClockPoint
	PlaybackClock() alsapcm.ClockPoint
}

// tcMedia ties the capture loop's stream positions to the device's clock
// (tcMu).
var tcMedia struct {
	dev   streamClocks
	rate  int
	offRx int64 // capture stream position minus the capture stream's appl count
}

// tcMediaActive reports whether the capture loop's device reports clocks.
func tcMediaActive() bool {
	tcMu.Lock()
	defer tcMu.Unlock()
	return tcMedia.dev != nil
}

// tcNoteCapture is called by the capture loop after every read: pos is its
// stream position after the read.
func tcNoteCapture(dev any, pos int64, rate int) {
	c, ok := dev.(streamClocks)
	if !ok {
		return
	}
	cp := c.CaptureClock()
	if !cp.Valid {
		return
	}
	tcMu.Lock()
	tcMedia.dev, tcMedia.rate, tcMedia.offRx = c, rate, pos-cp.Appl
	tcMu.Unlock()
}

// tcMediaForget drops the device (it is closing).
func tcMediaForget(dev any) {
	tcMu.Lock()
	defer tcMu.Unlock()
	if c, ok := dev.(streamClocks); ok && tcMedia.dev == c {
		tcMedia.dev = nil
	}
}

func hwAt(cp alsapcm.ClockPoint, t time.Time, rate int) float64 {
	return float64(cp.Hw) + t.Sub(cp.At).Seconds()*float64(rate)
}

// tcRxPosAtLocked is the capture stream position being captured at wall
// time t. Caller holds tcMu.
func tcRxPosAtLocked(t time.Time) (float64, bool) {
	m := &tcMedia
	if m.dev == nil {
		return 0, false
	}
	cp := m.dev.CaptureClock()
	if !cp.Valid {
		return 0, false
	}
	return hwAt(cp, t, m.rate) + float64(m.offRx), true
}

// tcRxPosOfTxLocked is the capture stream position at the media instant
// at which transmit appl position q (the next frame a write puts on the
// wire) is played. Caller holds tcMu.
func tcRxPosOfTxLocked(q int64) (float64, bool) {
	m := &tcMedia
	if m.dev == nil {
		return 0, false
	}
	cp, pp := m.dev.CaptureClock(), m.dev.PlaybackClock()
	if !cp.Valid || !pp.Valid {
		return 0, false
	}
	t := cp.At
	if pp.At.After(t) {
		t = pp.At
	}
	// Same instant on both streams: capture hw - playback hw is the
	// offset between their start times.
	d := hwAt(cp, t, m.rate) - hwAt(pp, t, m.rate)
	x := float64(q) + d + float64(m.offRx)
	// Received LTC is inferno's receive latency old by the time it is
	// captured: what the source sends at a media instant is the code
	// captured that much later. (MTC does not travel through it.)
	if int(tcLive.source.Load()) == tcSourceLTC {
		x += float64(rxLatencyLive.Load()) / 1e9 * float64(m.rate)
	}
	return x, true
}

// The receive latency (rxlatency.go: the running instance's RX_LATENCY_NS)
// is how far behind the media instant it was sent at every received sample
// is captured - the TIMECODE channel included. Takes record audio and code
// with the same delay, so their stamps need no correction; the chase and
// the relay, which transmit against the code as the source sends it, do.
// Measured on the network: without it both trailed the source by 9.5 ms
// at 10 ms. (A sender that asks for more than the unit's latency raises its
// flow's latency above this, and the chase then trails by the difference.)

// tcCodeAtRxLocked is the selected input's code position (frames since
// midnight) at capture stream position x, and its rate. ok is false when
// the input is not locked there. Caller holds tcMu.
func tcCodeAtRxLocked(x float64) (frames float64, rate int, ok bool) {
	switch int(tcLive.source.Load()) {
	case tcSourceLTC:
		l := &tcLTC
		if !l.has || l.run < tcLockFrames || l.sr == 0 || l.stream != tcMediaStreamLocked() {
			return 0, 0, false
		}
		ahead := x - float64(l.endPos)
		if ahead > float64(l.sr)*tcStaleAfter.Seconds() || ahead < -float64(l.sr) {
			return 0, 0, false
		}
		r := tcRates[l.rate]
		return float64(l.nextFrames) + tcFramesOfSeconds(ahead/float64(l.sr), r), l.rate, true
	case tcSourceMTC:
		m := &tcMTC
		if !m.running || m.run < 2 || len(m.hist) == 0 || tcMedia.rate == 0 {
			return 0, 0, false
		}
		last := m.hist[len(m.hist)-1]
		if x-last.x > float64(tcMedia.rate)*tcStaleAfter.Seconds() {
			return 0, 0, false
		}
		// Labels arrive with network jitter; the mean offset of the recent
		// ones (code minus position, at the code's own rate) cancels most
		// of it.
		r := tcRates[m.rate]
		perSample := tcFramesOfSeconds(1/float64(tcMedia.rate), r)
		var sum float64
		for _, h := range m.hist {
			sum += h.frames - h.x*perSample
		}
		return x*perSample + sum/float64(len(m.hist)), m.rate, true
	}
	return 0, 0, false
}

// tcMediaStreamLocked is the capture loop's stream identity for the LTC
// reader (the device), nil without one.
func tcMediaStreamLocked() any {
	if tcMedia.dev == nil {
		return nil
	}
	return any(tcMedia.dev)
}

// mtcAnchor is one MTC label placed on the capture stream.
type mtcAnchor struct {
	x      float64 // capture stream position when it completed
	frames float64 // code position then
}

// tcMTCHistory is how many labels the MTC position is averaged over: 16
// labels are 1.3 s at 25 fps.
const tcMTCHistory = 16
