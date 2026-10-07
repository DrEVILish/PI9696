package main

import (
	"fmt"
	"strconv"
	"strings"
)

// SMPTE timecode: values, rates and the arithmetic between them.
//
// A timecode is addressed by its frame count since midnight; seconds and
// sample positions follow from the rate's real frame rate (30000/1001 for
// the 29.97 rates). Drop-frame numbering (29.97 DF) skips frame numbers 0
// and 1 at the start of every minute except each tenth, so its labels stay
// within 3.6 ms per hour of the wall clock; the frames themselves are not
// dropped.

// tcRate is one selectable timecode rate.
type tcRate struct {
	Name    string // as shown in the UI
	Nominal int    // frames per labelled second: 24, 25 or 30
	Num     int64  // real frame rate is Num/Den frames per second
	Den     int64
	Drop    bool // drop-frame numbering (29.97 DF only)
}

// tcRates are the rates the generator offers and the decoders recognise.
// The default (index 1) is 25 fps, the broadcast rate at 48 kHz in most of
// the world.
var tcRates = []tcRate{
	{"23.976", 24, 24000, 1001, false},
	{"24", 24, 24, 1, false},
	{"25", 25, 25, 1, false},
	{"29.97 DF", 30, 30000, 1001, true},
	{"29.97", 30, 30000, 1001, false},
	{"30", 30, 30, 1, false},
}

const tcRateDefault = 2 // 25 fps

// tcRateFor picks the rate that labels like nominal/drop describe, given
// pulled tells whether the frames run 1000/1001 slow (23.976, 29.97). A
// decoder knows nominal and drop from the code itself and pulled from the
// measured frame period.
func tcRateFor(nominal int, drop, pulled bool) int {
	for i, r := range tcRates {
		if r.Nominal == nominal && r.Drop == drop && (r.Den == 1001) == pulled {
			return i
		}
	}
	// Drop-frame labels only exist at 29.97; a non-pulled DF reading is a
	// slightly fast source.
	for i, r := range tcRates {
		if r.Nominal == nominal && r.Drop == drop {
			return i
		}
	}
	return -1
}

// framesPerDay is the frame count of 24 hours of labels at r.
func (r tcRate) framesPerDay() int64 {
	if r.Drop {
		// 108 frame numbers dropped per hour (2 in 54 of every 60 minutes).
		return 24 * (108000 - 108)
	}
	return 24 * 3600 * int64(r.Nominal)
}

// Timecode is an hh:mm:ss:ff label.
type Timecode struct {
	H, M, S, F int
}

func (t Timecode) String() string {
	return fmt.Sprintf("%02d:%02d:%02d:%02d", t.H, t.M, t.S, t.F)
}

// format writes the label with ';' before the frames for drop-frame rates,
// the usual convention.
func (t Timecode) format(r tcRate) string {
	if r.Drop {
		return fmt.Sprintf("%02d:%02d:%02d;%02d", t.H, t.M, t.S, t.F)
	}
	return t.String()
}

// valid reports whether t is a label that exists at r.
func (t Timecode) valid(r tcRate) bool {
	if t.H < 0 || t.H > 23 || t.M < 0 || t.M > 59 || t.S < 0 || t.S > 59 || t.F < 0 || t.F >= r.Nominal {
		return false
	}
	return !(r.Drop && t.S == 0 && t.F < 2 && t.M%10 != 0)
}

// parseTimecode reads "hh:mm:ss:ff" (or with ';' or '.' before ff).
func parseTimecode(s string) (Timecode, bool) {
	f := strings.FieldsFunc(strings.TrimSpace(s), func(r rune) bool { return r == ':' || r == ';' || r == '.' })
	if len(f) != 4 {
		return Timecode{}, false
	}
	var v [4]int
	for i, p := range f {
		n, err := strconv.Atoi(p)
		if err != nil || len(p) > 2 {
			return Timecode{}, false
		}
		v[i] = n
	}
	return Timecode{v[0], v[1], v[2], v[3]}, true
}

// frames is t's frame count since midnight at r.
func (t Timecode) frames(r tcRate) int64 {
	n := int64(r.Nominal)
	f := ((int64(t.H)*60+int64(t.M))*60+int64(t.S))*n + int64(t.F)
	if r.Drop {
		mins := int64(t.H)*60 + int64(t.M)
		f -= 2 * (mins - mins/10)
	}
	return f
}

// timecodeAt is the label of frame count f since midnight at r (wrapping
// at 24 hours, either way).
func timecodeAt(f int64, r tcRate) Timecode {
	day := r.framesPerDay()
	f = ((f % day) + day) % day
	if r.Drop {
		// Re-insert the skipped labels: 17982 frames per ten minutes, the
		// first minute of each ten keeps all 1800, the other nine 1798.
		d, m := f/17982, f%17982
		f += 18 * d
		if m >= 2 {
			f += 2 * ((m - 2) / 1798)
		}
	}
	n := int64(r.Nominal)
	return Timecode{
		H: int(f / (3600 * n)),
		M: int(f / (60 * n) % 60),
		S: int(f / n % 60),
		F: int(f % n),
	}
}

// add returns the label k frames later (earlier for negative k).
func (t Timecode) add(k int64, r tcRate) Timecode {
	return timecodeAt(t.frames(r)+k, r)
}

// samplesFromFrames converts a frame count since midnight into a sample
// count at sampleRate: the BWF time reference of that frame's start.
func samplesFromFrames(f int64, r tcRate, sampleRate int) int64 {
	return f * r.Den * int64(sampleRate) / r.Num
}

// framesFromSamples is samplesFromFrames' inverse, rounding down: the frame
// a sample position falls in.
func framesFromSamples(s int64, r tcRate, sampleRate int) int64 {
	num := s * r.Num
	den := r.Den * int64(sampleRate)
	q := num / den
	if num%den != 0 && num < 0 {
		q--
	}
	return q
}

// tcSecondsOfFrames is the real time of f frames at r, in seconds.
func tcSecondsOfFrames(f float64, r tcRate) float64 {
	return f * float64(r.Den) / float64(r.Num)
}

// tcFramesOfSeconds is tcSecondsOfFrames' inverse.
func tcFramesOfSeconds(s float64, r tcRate) float64 {
	return s * float64(r.Num) / float64(r.Den)
}
