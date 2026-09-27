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
