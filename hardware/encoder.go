package hardware

import (
	"fmt"
	"sync"
	"time"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
)

type Encoder struct {
	pinA       gpio.PinIn
	pinB       gpio.PinIn
	pinButton  gpio.PinIn
	lastA      gpio.Level
	lastB      gpio.Level
	position   int
	buttonDown bool
	buttonTime time.Time
	mutex      sync.Mutex
	callbacks  struct {
		onRotate func(direction int)  // +1 for clockwise, -1 for counter-clockwise
		onClick  func()
		onHold   func() // Called after 3 second hold
	}
}

func NewEncoder() (*Encoder, error) {
	pinA := gpioreg.ByName("GPIO17")
	if pinA == nil {
		return nil, fmt.Errorf("failed to get encoder pin A")
	}
	if err := pinA.In(gpio.PullUp, gpio.BothEdges); err != nil {
		return nil, fmt.Errorf("failed to configure encoder pin A: %v", err)
	}

	pinB := gpioreg.ByName("GPIO27")
	if pinB == nil {
		return nil, fmt.Errorf("failed to get encoder pin B")
	}
	if err := pinB.In(gpio.PullUp, gpio.BothEdges); err != nil {
		return nil, fmt.Errorf("failed to configure encoder pin B: %v", err)
	}

	pinButton := gpioreg.ByName("GPIO22")
	if pinButton == nil {
		return nil, fmt.Errorf("failed to get encoder button pin")
	}
	if err := pinButton.In(gpio.PullUp, gpio.BothEdges); err != nil {
		return nil, fmt.Errorf("failed to configure encoder button pin: %v", err)
	}

	e := &Encoder{
		pinA:      pinA,
		pinB:      pinB,
		pinButton: pinButton,
		lastA:     pinA.Read(),
		lastB:     pinB.Read(),
		position:  0,
	}

	// Start monitoring goroutine
	go e.monitor()

	return e, nil
}

func (e *Encoder) monitor() {
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		e.readEncoder()
		e.readButton()
	}
}

func (e *Encoder) readEncoder() {
	currentA := e.pinA.Read()
	currentB := e.pinB.Read()

	if currentA != e.lastA {
		if currentA == gpio.Low {
			// Falling edge on A
			if currentB == gpio.Low {
				// B is also low, clockwise
				e.handleRotation(1)
			} else {
				// B is high, counter-clockwise
				e.handleRotation(-1)
			}
		}
	}

	e.lastA = currentA
	e.lastB = currentB
}

func (e *Encoder) readButton() {
	currentButton := e.pinButton.Read() == gpio.Low // Active low

	e.mutex.Lock()
	defer e.mutex.Unlock()

	if currentButton && !e.buttonDown {
		// Button pressed
		e.buttonDown = true
		e.buttonTime = time.Now()
	} else if !currentButton && e.buttonDown {
		// Button released
		e.buttonDown = false
		holdTime := time.Since(e.buttonTime)
		
		if holdTime >= 3*time.Second {
			// Long press (3+ seconds)
			if e.callbacks.onHold != nil {
				go e.callbacks.onHold()
			}
		} else if holdTime >= 50*time.Millisecond {
			// Normal click (debounced)
			if e.callbacks.onClick != nil {
				go e.callbacks.onClick()
			}
		}
	}
}

func (e *Encoder) handleRotation(direction int) {
	e.mutex.Lock()
	e.position += direction
	callback := e.callbacks.onRotate
	e.mutex.Unlock()

	if callback != nil {
		go callback(direction)
	}
}

func (e *Encoder) GetPosition() int {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.position
}

func (e *Encoder) ResetPosition() {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.position = 0
}

func (e *Encoder) SetRotateCallback(callback func(direction int)) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.callbacks.onRotate = callback
}

func (e *Encoder) SetClickCallback(callback func()) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.callbacks.onClick = callback
}

func (e *Encoder) SetHoldCallback(callback func()) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.callbacks.onHold = callback
}

func (e *Encoder) IsButtonPressed() bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.buttonDown
}