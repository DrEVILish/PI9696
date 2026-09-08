package hardware

import (
	"fmt"
	"log/slog"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"sync"
)

type LampType int

const (
	RecLamp LampType = iota
	PlayLamp
)

// LampManager drives the REC and PLAY button backlights (the Round 3
// redesign: backlights replace the old GPIO status LEDs; STOP has no lamp).
// Each Set writes its pin only when the requested state actually changes, so
// the app can call it every render tick without churning the GPIO. In sim
// mode there are no pins and everything is a no-op.
//
// Pins: REC = GPIO12, PLAY = GPIO16 (the old STOP/status pin, freed when the
// STOP lamp was dropped). See WIRING.md.
type LampManager struct {
	mu     sync.Mutex
	pins   [2]gpio.PinOut
	states [2]bool
}

func NewLampManager() (*LampManager, error) {
	lm := &LampManager{}
	if simMode() {
		return lm, nil
	}
	for kind, name := range map[LampType]string{RecLamp: "GPIO12", PlayLamp: "GPIO16"} {
		pin := gpioreg.ByName(name)
		if pin == nil {
			return nil, fmt.Errorf("failed to get %s lamp pin", name)
		}
		out, ok := pin.(gpio.PinOut)
		if !ok {
			return nil, fmt.Errorf("%s is not an output pin", name)
		}
		if err := out.Out(gpio.Low); err != nil {
			return nil, fmt.Errorf("failed to configure %s lamp pin: %v", name, err)
		}
		lm.pins[kind] = out
		lm.states[kind] = false
	}
	return lm, nil
}

// Set turns a lamp on/off, writing the pin only when the state changes.
func (lm *LampManager) Set(kind LampType, on bool) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if lm.pins[kind] == nil || lm.states[kind] == on {
		return
	}
	lm.states[kind] = on
	level := gpio.Low
	if on {
		level = gpio.High
	}
	if err := lm.pins[kind].Out(level); err != nil {
		slog.Warn(fmt.Sprintf("lamp %d: %v", kind, err))
	}
}

// Close turns every lamp off and releases the pins.
func (lm *LampManager) Close() {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	for i := range lm.pins {
		if lm.pins[i] != nil && lm.states[i] {
			lm.pins[i].Out(gpio.Low)
			lm.states[i] = false
		}
	}
}