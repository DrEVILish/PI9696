package hardware

import (
	"testing"

	"periph.io/x/conn/v3/physic"
)

// The SSD1322 is clocked at 10MHz (WIRING.md: below that the panel garbles).
// physic.Frequency is scaled with Hertz == 1e6, so the clock has to be written
// with a unit constant - a bare 10000000 literal is 10Hz, which the sysfs SPI
// driver rejects, taking the whole display init (and with it startup) down.
// SIM mode never opens SPI, so nothing else would have caught this.
func TestSPIClockIs10MHz(t *testing.T) {
	const want = physic.Frequency(10 * physic.MegaHertz)
	got := spiClock
	if got != want {
		t.Fatalf("SPI clock = %v, want %v", got, want)
	}
	// Guard the unit conversion itself, so the literal above cannot be
	// "simplified" back into a raw count without the test noticing.
	if physic.Hertz != 1000000 {
		t.Fatalf("periph scaled physic.Hertz to %d, not 1e6 - revisit the SPI clock literal", physic.Hertz)
	}
}

// A 256x64 4bpp frame is 8192 bytes, twice what the sysfs SPI driver accepts
// in one transfer. If this ever stops being true the chunking in writeData is
// dead code, and if the panel grows the chunking silently becomes insufficient
// again - so assert the relationship rather than the constant.
func TestFramebufferExceedsSPITxLimit(t *testing.T) {
	frame := DisplayWidth * DisplayHeight / 2
	if frame <= maxSPITx {
		t.Fatalf("framebuffer is %d bytes, no longer over the %d byte SPI limit - chunking is now pointless", frame, maxSPITx)
	}
	if frame%(maxSPITx/2) != 0 {
		t.Fatalf("framebuffer %d is not a whole number of %d byte chunks", frame, maxSPITx)
	}
}
