package hardware

import (
	"errors"
	"testing"

	"periph.io/x/conn/v3/gpio"
)

// errPin is a gpio.PinOut whose Out always fails. Only Out is ever called;
// the embedded (nil) interface would panic on anything else, which is exactly
// what we want a test to do loudly rather than silently pass.
type errPin struct {
	gpio.PinOut
	err error
}

func (p errPin) Out(l gpio.Level) error { return p.err }

// A dead DC pin must fail the transfer loudly instead of sending command
// bytes with the panel listening for data (or vice versa). writeData must
// fail before touching SPI, so a nil conn is safe here.
func TestWriteCommandReportsDCPinFailure(t *testing.T) {
	d := &TTFDisplay{dcPin: errPin{err: errors.New("boom")}}
	if err := d.writeCommand([]byte{0xAE}); err == nil {
		t.Fatal("writeCommand with a dead DC pin returned no error")
	}
	if err := d.writeData([]byte{0x00}); err == nil {
		t.Fatal("writeData with a dead DC pin returned no error")
	}
}

// Same for the reset line: init must report it instead of running the whole
// init sequence against an unreset panel.
func TestInitReportsResetPinFailure(t *testing.T) {
	d := &TTFDisplay{
		resPin: errPin{err: errors.New("boom")},
		dcPin:  errPin{err: errors.New("boom")},
	}
	if err := d.init(); err == nil {
		t.Fatal("init with a dead reset pin returned no error")
	}
}

// truncateRunes must count characters, not bytes: maxChars is a character
// budget while len() counts bytes, so byte slicing a multi-byte name splits
// UTF-8 mid-sequence and renders as garbage.
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abcdef", 10); got != "abcdef" {
		t.Errorf("short string = %q, want untouched", got)
	}
	if got := truncateRunes("abcdef", 4); got != "abcd..." {
		t.Errorf("long string = %q, want abcd...", got)
	}
	// "é" is 2 bytes: byte slicing at 4 would split it, rune slicing keeps it.
	if got := truncateRunes("abédef", 4); got != "abéd..." {
		t.Errorf("multibyte string = %q, want abéd...", got)
	}
	for _, s := range []string{"abédef", "日本語テスト", "a🎵b"} {
		got := truncateRunes(s, 3)
		for i := range got {
			_ = i
		}
		// Must remain valid UTF-8 no matter where the cut lands.
		for _, r := range got {
			if r == 0xFFFD && len(got) > 0 {
				t.Errorf("%q produced invalid UTF-8: %q", s, got)
			}
		}
	}
}
