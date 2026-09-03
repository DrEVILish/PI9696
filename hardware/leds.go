package hardware

import (
	"fmt"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
)

// LED is a simple digital-output status indicator - no PWM/brightness
// control, just on/off, matching what a status LED on a rack recorder needs.
type LED struct {
	pin gpio.PinOut
}

// Set turns the LED on or off. Safe to call on a sim-mode LED (nil pin).
func (l *LED) Set(on bool) {
	if l == nil || l.pin == nil {
		return
	}
	level := gpio.Low
	if on {
		level = gpio.High
	}
	l.pin.Out(level)
}

type LEDManager struct {
	Record *LED // solid while a recording is in progress
	Status *LED // solid while the Inferno server is running
}

// NewLEDManager initializes the two status LEDs. GPIO12 (Record) and GPIO16
// (Status) were picked to avoid every pin already spoken for: GPIO5/6/13
// (buttons), GPIO17/22/27 (encoder), GPIO8/10/11 (display SPI), GPIO24/25
// (display DC/RES), GPIO2/3 (I2C, enabled by setup.sh's raspi-config step),
// and GPIO14/15/18-21 (UART and I2S, left clear for an audio HAT).
// (Record→GPIO12, Status→GPIO16 matches the documented WIRING.md/README/PROJECT_STATUS spec.)
func NewLEDManager() (*LEDManager, error) {
	if simMode() {
		// No real GPIO on a dev machine; LED.Set on a nil pin is a no-op, so
		// the rest of the app can call it unconditionally.
		return &LEDManager{Record: &LED{}, Status: &LED{}}, nil
	}

	recordPin := gpioreg.ByName("GPIO12")
	if recordPin == nil {
		return nil, fmt.Errorf("failed to get record LED pin")
	}
	if err := recordPin.Out(gpio.Low); err != nil {
		return nil, fmt.Errorf("failed to configure record LED pin: %v", err)
	}

	statusPin := gpioreg.ByName("GPIO16")
	if statusPin == nil {
		return nil, fmt.Errorf("failed to get status LED pin")
	}
	if err := statusPin.Out(gpio.Low); err != nil {
		return nil, fmt.Errorf("failed to configure status LED pin: %v", err)
	}

	return &LEDManager{
		Record: &LED{pin: recordPin},
		Status: &LED{pin: statusPin},
	}, nil
}
