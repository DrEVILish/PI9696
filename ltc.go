package main

// SMPTE linear timecode (LTC, SMPTE 12M) as audio samples: the encoder the
// TIMECODE transmit channel carries, and the decoder for the TIMECODE
// receive channel and recorded timecode tracks.
//
// A frame is 80 bits sent bit 0 first in biphase mark code: the level flips
// at every bit boundary, and once more mid-bit for a 1. Bits 64-79 are the
// sync word 0011 1111 1111 1101, which marks the frame's end and its
// direction. One bit per frame is chosen so the frame holds an even number
// of 1 bits; every frame then starts at the same level, so a frame's
// waveform depends on its label alone.

const (
	ltcBits     = 80
	ltcSyncWord = 0xBFFC // bits 64..79 as sent, bit 64 in bit 0
)

// ltcLevel is the encoder's peak level: about -12 dBFS, on a 24-bit grid
// (the low byte is zero; the transmitter sends 24-bit samples untouched).
const ltcLevel = int32(0x20000000)

// ltcPolarityBit is the bit that evens out the frame's 1 bits: bit 59 at
// 25 fps, bit 27 at the other rates (the two swap places in SMPTE 12M).
func ltcPolarityBit(r tcRate) int {
	if r.Nominal == 25 {
		return 59
	}
	return 27
}

// ltcFrameBits packs t into the 80 bits of an LTC frame, bit i of the
// result's [lo, hi] pair being frame bit i (lo bits 0-63, hi bits 64-79).
// User bits are zero.
func ltcFrameBits(t Timecode, r tcRate) (lo uint64, hi uint16) {
	put := func(at int, v int, n int) {
		lo |= uint64(v&(1<<n-1)) << at
	}
	put(0, t.F%10, 4)
	put(8, t.F/10, 2)
	if r.Drop {
		lo |= 1 << 10
	}
	put(16, t.S%10, 4)
	put(24, t.S/10, 3)
	put(32, t.M%10, 4)
	put(40, t.M/10, 3)
	put(48, t.H%10, 4)
	put(56, t.H/10, 2)
	hi = ltcSyncWord
	ones := popcount64(lo) + popcount64(uint64(hi))
	if ones%2 != 0 {
		lo |= 1 << ltcPolarityBit(r)
	}
	return lo, hi
}

func popcount64(x uint64) int {
	n := 0
	for ; x != 0; x &= x - 1 {
		n++
	}
	return n
}

// ltcFrameFromBits unpacks a frame's data bits. ok is false when a field
// is out of range (noise that happened to end in a sync word).
func ltcFrameFromBits(lo uint64) (t Timecode, drop bool, ok bool) {
	get := func(at, n int) int { return int(lo>>at) & (1<<n - 1) }
	fu, ft := get(0, 4), get(8, 2)
	su, st := get(16, 4), get(24, 3)
	mu, mt := get(32, 4), get(40, 3)
	hu, ht := get(48, 4), get(56, 2)
	if fu > 9 || su > 9 || mu > 9 || hu > 9 || st > 5 || mt > 5 {
		return Timecode{}, false, false
	}
	t = Timecode{H: ht*10 + hu, M: mt*10 + mu, S: st*10 + su, F: ft*10 + fu}
	if t.H > 23 || t.F > 29 {
		return Timecode{}, false, false
	}
	return t, lo&(1<<10) != 0, true
}

// ltcEncoder renders LTC for absolute sample positions: sample n belongs to
// the frame labelled timecodeAt(frame) where frame = n * rate / sampleRate,
// so any stream position maps to its sample without state. It caches the
// half-bit levels of the last frame it rendered.
type ltcEncoder struct {
	rate       tcRate
	sampleRate int
	frame      int64 // frame whose levels are cached; -1 none
	levels     [2 * ltcBits]int8
}

func newLTCEncoder(r tcRate, sampleRate int) *ltcEncoder {
	return &ltcEncoder{rate: r, sampleRate: sampleRate, frame: -1}
}

// level is +1/-1: the code's level during sample n.
func (e *ltcEncoder) level(n int64) int8 {
	// Half-bits since midnight; int64 holds a day at 192 kHz with room.
	hb := n * e.rate.Num * 2 * ltcBits / (e.rate.Den * int64(e.sampleRate))
	if n < 0 && (n*e.rate.Num*2*ltcBits)%(e.rate.Den*int64(e.sampleRate)) != 0 {
		hb--
	}
	frame := hb / (2 * ltcBits)
	h := hb - frame*2*ltcBits
	if h < 0 {
		h += 2 * ltcBits
		frame--
	}
	if frame != e.frame {
		e.render(frame)
	}
	return e.levels[h]
}

func (e *ltcEncoder) render(frame int64) {
	lo, hi := ltcFrameBits(timecodeAt(frame, e.rate), e.rate)
	lvl := int8(-1) // the level before every frame (even transitions each)
	for b := 0; b < ltcBits; b++ {
		var one bool
		if b < 64 {
			one = lo>>b&1 != 0
		} else {
			one = hi>>(b-64)&1 != 0
		}
		lvl = -lvl // boundary transition
		e.levels[2*b] = lvl
		if one {
			lvl = -lvl
		}
		e.levels[2*b+1] = lvl
	}
	e.frame = frame
}

// sample is the output value at absolute sample n: the level, with a one-
// sample step at each transition (the code's 25 us rise time is about one
// sample at 48 kHz).
func (e *ltcEncoder) sample(n int64) int32 {
	a, b := e.level(n), e.level(n-1)
	return int32(a+b) * (ltcLevel / 2)
}

// fill writes LTC for absolute samples start.. into channel ch of the
// interleaved buffer buf (frames of stride channels).
func (e *ltcEncoder) fill(buf []int32, channels, ch int, start int64) {
	for i := 0; i*channels+ch < len(buf); i++ {
		buf[i*channels+ch] = e.sample(start + int64(i))
	}
}

// ltcDecoded is one frame the decoder found: its label, and the sample
// position (in the decoder's own count) where the frame ended.
type ltcDecoded struct {
	tc     Timecode
	drop   bool
	endPos int64
	// rate is the decoder's best reading of the frame rate (an index into
	// tcRates), -1 until it has seen enough frames to tell.
	rate int
}

// ltcDecoder finds LTC frames in a stream of samples. It needs no setup
// beyond the sample rate: the bit period is tracked from the signal, so
// any rate from 23.976 to 30 fps (and moderate varispeed) decodes.
type ltcDecoder struct {
	sampleRate int
	pos        int64   // samples seen
	high       bool    // current level
	lastEdge   int64   // position of the last transition
	period     float64 // estimated bit period in samples
	halfPend   bool    // first half of a 1 seen
	lo         uint64  // last 80 bits, oldest at lo bit 0
	hi         uint16
	nbits      int
	lastEnd    int64   // end position of the previous decoded frame, -1 none
	frameLens  []int64 // recent frame lengths in samples, for the rate
	maxFrame   int
	threshold  int32
}

func newLTCDecoder(sampleRate int) *ltcDecoder {
	return &ltcDecoder{
		sampleRate: sampleRate,
		period:     float64(sampleRate) / 2000, // 25 fps; adapts within bits
		lastEnd:    -1,
		threshold:  1 << 24, // about -42 dBFS: below this is not a level change
	}
}

// feed consumes samples, calling found for every complete frame.
func (d *ltcDecoder) feed(samples []int32, stride, ch int, found func(ltcDecoded)) {
	for i := ch; i < len(samples); i += stride {
		x := samples[i]
		d.pos++
		switch {
		case !d.high && x > d.threshold:
			d.high = true
		case d.high && x < -d.threshold:
			d.high = false
		default:
			continue
		}
		d.edge(d.pos-d.lastEdge, found)
		d.lastEdge = d.pos
	}
}

// edge classifies the interval since the previous transition: about a bit
// period is a 0, about half of one is half of a 1.
func (d *ltcDecoder) edge(gap int64, found func(ltcDecoded)) {
	g := float64(gap)
	switch {
	case g > d.period*1.6 || g < d.period*0.3:
		// Not LTC timing (silence, a dropout, noise): start over, but keep
		// the period if the gap is a plausible bit at another rate.
		d.halfPend = false
		d.nbits = 0
		if g >= float64(d.sampleRate)/2600 && g <= float64(d.sampleRate)/1800 {
			d.period = g
		}
	case g > d.period*0.75:
		d.period = 0.8*d.period + 0.2*g
		if d.halfPend {
			// A full period after a lone half: the halves were misread.
			d.halfPend = false
			d.nbits = 0
			return
		}
		d.bit(0, found)
	default:
		d.period = 0.8*d.period + 0.2*2*g
		if d.halfPend {
			d.halfPend = false
			d.bit(1, found)
		} else {
			d.halfPend = true
		}
	}
}

func (d *ltcDecoder) bit(b uint64, found func(ltcDecoded)) {
	d.lo = d.lo>>1 | uint64(d.hi&1)<<63
	d.hi = d.hi>>1 | uint16(b)<<15
	d.nbits++
	if d.nbits < ltcBits || d.hi != ltcSyncWord {
		return
	}
	tc, drop, ok := ltcFrameFromBits(d.lo)
	if !ok {
		return
	}
	// A bit is complete at the edge that ends it, so this edge is the
	// frame's end: the next frame's first boundary. pos-1 is the sample
	// that crossed the threshold.
	end := d.pos - 1
	if d.lastEnd >= 0 {
		if l := end - d.lastEnd; l > 0 && l < int64(d.sampleRate) {
			d.frameLens = append(d.frameLens, l)
			if len(d.frameLens) > 32 {
				d.frameLens = d.frameLens[1:]
			}
		}
	}
	d.lastEnd = end
	if tc.F > d.maxFrame {
		d.maxFrame = tc.F
	}
	found(ltcDecoded{tc: tc, drop: drop, endPos: end, rate: d.rate(drop)})
}

// rate reads the frame rate from the measured frame length: nominal 24, 25
// or 30 by nearest match, then pulled-down (x1000/1001) or not by which of
// the two lengths the average is closer to. The highest frame number seen
// settles 24/25/30 too, and wins once it has proved the rate is higher than
// the length suggests (varispeed).
func (d *ltcDecoder) rate(drop bool) int {
	if len(d.frameLens) < 8 {
		return -1
	}
	var sum int64
	for _, l := range d.frameLens {
		sum += l
	}
	avg := float64(sum) / float64(len(d.frameLens))
	best, bestErr := -1, 0.0
	for i, r := range tcRates {
		if r.Drop != drop {
			continue
		}
		want := float64(d.sampleRate) * float64(r.Den) / float64(r.Num)
		e := (avg - want) / want
		if e < 0 {
			e = -e
		}
		if best < 0 || e < bestErr {
			best, bestErr = i, e
		}
	}
	if best >= 0 && d.maxFrame >= tcRates[best].Nominal {
		if alt := tcRateFor(d.maxFrame+1, drop, tcRates[best].Den == 1001); alt >= 0 {
			return alt
		}
	}
	return best
}
