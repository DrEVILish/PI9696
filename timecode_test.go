package main

import (
	"testing"
)

func TestTimecodeFramesRoundTrip(t *testing.T) {
	for ri, r := range tcRates {
		day := r.framesPerDay()
		for _, f := range []int64{0, 1, 2, r.framesPerDay() - 1, 1799, 1800, 1801, 17981, 17982, 17983, 107892, 1234567 % day} {
			tc := timecodeAt(f, r)
			if !tc.valid(r) {
				t.Fatalf("%s: frame %d -> %v, not a valid label", r.Name, f, tc)
			}
			if back := tc.frames(r); back != f {
				t.Fatalf("%s (%d): frame %d -> %v -> %d", r.Name, ri, f, tc, back)
			}
		}
	}
}

func TestDropFrameLabels(t *testing.T) {
	r := tcRates[3]
	if !r.Drop {
		t.Fatal("rate 3 should be 29.97 DF")
	}
	cases := []struct {
		f    int64
		want string
	}{
		{1799, "00:00:59;29"},
		{1800, "00:01:00;02"}, // ;00 and ;01 skipped
		{17981, "00:09:59;29"},
		{17982, "00:10:00;00"}, // every tenth minute keeps them
		{107892, "01:00:00;00"},
	}
	for _, c := range cases {
		if got := timecodeAt(c.f, r).format(r); got != c.want {
			t.Errorf("frame %d = %s, want %s", c.f, got, c.want)
		}
	}
	if (Timecode{0, 1, 0, 0}).valid(r) || !(Timecode{0, 10, 0, 0}).valid(r) {
		t.Error("drop-frame validity wrong")
	}
	// One hour of drop-frame labels is 3.6 ms short of an hour.
	if s := tcSecondsOfFrames(float64(107892), r); s < 3599.99 || s > 3600.0 {
		t.Errorf("1 h DF = %.4f s", s)
	}
}

func TestTimecodeSamples(t *testing.T) {
	r := tcRates[tcRateDefault]
	tc := Timecode{10, 0, 0, 0}
	s := samplesFromFrames(tc.frames(r), r, 48000)
	if s != 10*3600*48000 {
		t.Fatalf("10:00:00:00 = %d samples", s)
	}
	if f := framesFromSamples(s+1919, r, 48000); f != tc.frames(r) {
		t.Fatalf("a sample inside the frame maps to frame %d", f)
	}
	if f := framesFromSamples(s+1920, r, 48000); f != tc.frames(r)+1 {
		t.Fatalf("the next frame's first sample maps to %d", f)
	}
	if tc.add(-1, r) != (Timecode{9, 59, 59, 24}) {
		t.Fatal("add -1 wrong")
	}
	if (Timecode{0, 0, 0, 0}).add(-1, r) != (Timecode{23, 59, 59, 24}) {
		t.Fatal("midnight wrap wrong")
	}
	if got, ok := parseTimecode("01:02:03;04"); !ok || got != (Timecode{1, 2, 3, 4}) {
		t.Fatalf("parse = %v %v", got, ok)
	}
	for _, bad := range []string{"", "1:2:3", "01:02:03:004", "aa:00:00:00"} {
		if _, ok := parseTimecode(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestLTCFrameBits(t *testing.T) {
	for _, r := range tcRates {
		tc := Timecode{23, 59, 58, r.Nominal - 1}
		if r.Drop {
			tc = Timecode{12, 34, 56, 29}
		}
		lo, hi := ltcFrameBits(tc, r)
		if hi != ltcSyncWord {
			t.Fatalf("sync word %x", hi)
		}
		if (popcount64(lo)+popcount64(uint64(hi)))%2 != 0 {
			t.Fatalf("%s: odd number of 1 bits", r.Name)
		}
		got, drop, ok := ltcFrameFromBits(lo)
		if !ok || got != tc || drop != r.Drop {
			t.Fatalf("%s: %v -> %v drop=%v ok=%v", r.Name, tc, got, drop, ok)
		}
	}
}

// encodeLTC renders n samples of LTC starting at the label start.
func encodeLTC(r tcRate, sampleRate int, start Timecode, n int) []int32 {
	e := newLTCEncoder(r, sampleRate)
	buf := make([]int32, n)
	e.fill(buf, 1, 0, samplesFromFrames(start.frames(r), r, sampleRate))
	return buf
}

func TestLTCEncodeDecode(t *testing.T) {
	for ri, r := range tcRates {
		for _, sr := range []int{44100, 48000, 96000} {
			start := Timecode{10, 59, 58, 0}
			if r.Drop {
				start = Timecode{10, 8, 59, 20}
			}
			buf := encodeLTC(r, sr, start, sr*3)
			d := newLTCDecoder(sr)
			var got []ltcDecoded
			// Odd chunk sizes: the decoder must not care where buffers end.
			for off := 0; off < len(buf); off += 997 {
				d.feed(buf[off:min(off+997, len(buf))], 1, 0, func(f ltcDecoded) { got = append(got, f) })
			}
			fps := float64(r.Num) / float64(r.Den)
			if want := int(3*fps) - 2; len(got) < want {
				t.Fatalf("%s @%d: decoded %d frames, want >= %d", r.Name, sr, len(got), want)
			}
			// The first frame decoded may be the first or the second (the
			// decoder locks onto bit timing within a frame).
			first := got[0].tc.frames(r) - start.frames(r)
			if first < 0 || first > 1 {
				t.Fatalf("%s @%d: first frame %v", r.Name, sr, got[0].tc)
			}
			for i, f := range got {
				if want := start.add(first+int64(i), r); f.tc != want {
					t.Fatalf("%s @%d: frame %d = %v, want %v", r.Name, sr, i, f.tc, want)
				}
				if f.drop != r.Drop {
					t.Fatalf("%s: drop flag %v", r.Name, f.drop)
				}
				// The frame ends where the next one starts.
				wantEnd := samplesFromFrames(start.frames(r)+first+int64(i)+1, r, sr) - samplesFromFrames(start.frames(r), r, sr)
				if diff := f.endPos - wantEnd; diff < -2 || diff > 2 {
					t.Fatalf("%s @%d: frame %d ends at %d, want %d", r.Name, sr, i, f.endPos, wantEnd)
				}
			}
			if last := got[len(got)-1].rate; last != ri {
				t.Errorf("%s @%d: rate read as %d", r.Name, sr, last)
			}
		}
	}
}

func TestLTCDecodeInvertedAndQuiet(t *testing.T) {
	r := tcRates[tcRateDefault]
	buf := encodeLTC(r, 48000, Timecode{1, 0, 0, 0}, 48000)
	for i := range buf {
		buf[i] = -buf[i] / 16 // polarity flipped, 24 dB down
	}
	n := 0
	newLTCDecoder(48000).feed(buf, 1, 0, func(ltcDecoded) { n++ })
	if n < 23 {
		t.Fatalf("decoded %d frames of inverted, quiet LTC", n)
	}
	// Silence and noise below the threshold decode nothing.
	quiet := make([]int32, 48000)
	for i := range quiet {
		quiet[i] = int32(i*7919%1000) << 12
	}
	newLTCDecoder(48000).feed(quiet, 1, 0, func(ltcDecoded) { t.Fatal("decoded a frame from noise") })
}

func TestLTCEncoderInterleaved(t *testing.T) {
	r := tcRates[tcRateDefault]
	e := newLTCEncoder(r, 48000)
	buf := make([]int32, 3*480)
	e.fill(buf, 3, 2, 1000)
	for i := 0; i < 480; i++ {
		if buf[i*3] != 0 || buf[i*3+1] != 0 {
			t.Fatal("wrote outside its channel")
		}
		if want := e.sample(1000 + int64(i)); buf[i*3+2] != want {
			t.Fatal("fill differs from sample")
		}
		if v := buf[i*3+2]; v&0xFF != 0 {
			t.Fatalf("sample %x not on the 24-bit grid", v)
		}
	}
}

func TestMTCQuarterFrames(t *testing.T) {
	for ri, r := range tcRates {
		tc := Timecode{21, 43, 37, r.Nominal - 1}
		var d mtcDecoder
		var got mtcDecoded
		var n int
		// Start mid-sequence: the decoder waits for piece 0.
		for _, p := range []int{5, 6, 7, 0, 1, 2, 3, 4, 5, 6, 7} {
			if out, ok := d.quarterFrame(mtcQuarterFrame(p, tc, r)); ok {
				got, n = out, n+1
			}
		}
		if n != 1 || got.tc != tc || !got.running || got.offsetFrames != 1.75 {
			t.Fatalf("%s: n=%d got %+v", r.Name, n, got)
		}
		if tcRates[got.rate].Nominal != r.Nominal || tcRates[got.rate].Drop != r.Drop {
			t.Fatalf("%s (%d): rate read as %s", r.Name, ri, tcRates[got.rate].Name)
		}
		full, ok := d.sysEx(mtcFullFrame(tc, r))
		if !ok || full.tc != tc || full.running {
			t.Fatalf("%s: full frame %+v %v", r.Name, full, ok)
		}
	}
	var d mtcDecoder
	if _, ok := d.sysEx([]byte{0xF0, 0x7E, 0x7F, 0x06, 0x01, 0xF7}); ok {
		t.Fatal("decoded a non-MTC SysEx")
	}
}
