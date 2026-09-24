package hardware

import (
	"time"

	"periph.io/x/conn/v3/gpio"
)

// edgeTick waits for an edge on pin up to timeout. Drivers without edge
// support return instantly, which would spin a monitor loop; after 64
// consecutive instant wakeups it backs off to a short sleep, so the worst
// case degrades to a ~100Hz poll instead of a busy loop. Callers keep
// their existing read/debounce paths unchanged.
func edgeTick(pin gpio.PinIn, timeout time.Duration, spins *int) {
	start := time.Now()
	if pin.WaitForEdge(timeout) {
		*spins = 0
		return
	}
	if time.Since(start) < time.Millisecond {
		*spins++
		if *spins > 64 {
			time.Sleep(10 * time.Millisecond)
			*spins = 0
		}
	} else {
		*spins = 0
	}
}
