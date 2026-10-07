package main

// MIDI timecode (MTC): quarter-frame messages (0xF1 0nnndddd), eight of
// which carry one label over two frames, and the full-frame SysEx that
// announces a locate. The rate travels in the hours' top bits: 0 = 24,
// 1 = 25, 2 = 29.97 DF, 3 = 30 fps.

// mtcRateCode is r's two-bit MTC rate code (23.976 and 29.97 non-drop have
// none of their own and travel as 24 and 30).
func mtcRateCode(r tcRate) int {
	switch {
	case r.Nominal == 24:
		return 0
	case r.Nominal == 25:
		return 1
	case r.Drop:
		return 2
	default:
		return 3
	}
}

// mtcRateFromCode maps an MTC rate code back to a tcRates index.
func mtcRateFromCode(c int) int {
	switch c & 3 {
	case 0:
		return tcRateFor(24, false, false)
	case 1:
		return tcRateFor(25, false, false)
	case 2:
		return tcRateFor(30, true, true)
	default:
		return tcRateFor(30, false, false)
	}
}

// mtcQuarterFrame is the data byte of quarter-frame piece (0..7) of t.
func mtcQuarterFrame(piece int, t Timecode, r tcRate) byte {
	var v int
	switch piece {
	case 0:
		v = t.F & 0xF
	case 1:
		v = t.F >> 4 & 1
	case 2:
		v = t.S & 0xF
	case 3:
		v = t.S >> 4 & 3
	case 4:
		v = t.M & 0xF
	case 5:
		v = t.M >> 4 & 3
	case 6:
		v = t.H & 0xF
	case 7:
		v = t.H>>4&1 | mtcRateCode(r)<<1
	}
	return byte(piece<<4 | v)
}

// mtcFullFrame is the full-frame SysEx for t.
func mtcFullFrame(t Timecode, r tcRate) []byte {
	return []byte{0xF0, 0x7F, 0x7F, 0x01, 0x01, byte(mtcRateCode(r)<<5 | t.H), byte(t.M), byte(t.S), byte(t.F), 0xF7}
}

// mtcDecoded is a label the decoder assembled. offsetFrames is how far the
// source's position had moved past tc when the message completing it was
// sent: 1.75 frames for a quarter-frame sequence (the label is that of the
// frame piece 0 started), 0 for a full frame (a locate, not running).
type mtcDecoded struct {
	tc           Timecode
	rate         int
	offsetFrames float64
	running      bool
}

// mtcDecoder assembles labels from quarter-frame pieces and full frames.
type mtcDecoder struct {
	pieces [8]int
	next   int // the piece expected next; a forward sequence starts at 0
}

// quarterFrame takes one quarter-frame data byte; it returns a label when
// piece 7 completes a forward sequence 0..7.
func (d *mtcDecoder) quarterFrame(b byte) (mtcDecoded, bool) {
	piece, v := int(b>>4&7), int(b&0xF)
	if piece != d.next {
		// Out of sequence (a dropped packet, or reverse play): wait for
		// the next piece 0.
		d.next = 0
		if piece != 0 {
			return mtcDecoded{}, false
		}
	}
	d.pieces[piece] = v
	d.next = (piece + 1) % 8
	if piece != 7 {
		return mtcDecoded{}, false
	}
	p := d.pieces
	t := Timecode{
		F: p[0] | p[1]&1<<4,
		S: p[2] | p[3]&3<<4,
		M: p[4] | p[5]&3<<4,
		H: p[6] | p[7]&1<<4,
	}
	rate := mtcRateFromCode(p[7] >> 1)
	if rate < 0 || !t.valid(tcRates[rate]) {
		return mtcDecoded{}, false
	}
	return mtcDecoded{tc: t, rate: rate, offsetFrames: 1.75, running: true}, true
}

// sysEx takes a complete SysEx message (F0 .. F7); it returns a label for
// an MTC full frame.
func (d *mtcDecoder) sysEx(m []byte) (mtcDecoded, bool) {
	if len(m) != 10 || m[0] != 0xF0 || m[1] != 0x7F || m[3] != 0x01 || m[4] != 0x01 || m[9] != 0xF7 {
		return mtcDecoded{}, false
	}
	d.next = 0
	t := Timecode{H: int(m[5] & 0x1F), M: int(m[6]), S: int(m[7]), F: int(m[8])}
	rate := mtcRateFromCode(int(m[5]) >> 5)
	if rate < 0 || !t.valid(tcRates[rate]) {
		return mtcDecoded{}, false
	}
	return mtcDecoded{tc: t, rate: rate}, true
}
