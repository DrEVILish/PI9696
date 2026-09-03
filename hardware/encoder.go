package hardware

import (
	"fmt"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"sync"
	"time"
)

type Encoder struct {
	pinA             gpio.PinIn
	pinB             gpio.PinIn
	pinButton        gpio.PinIn
	lastA            gpio.Level
	lastB            gpio.Level
	lastRotationTime time.Time
	position         int
	buttonDown       bool
	buttonTime       time.Time
	pressPending     bool
	pressSince       time.Time
	releasePending   bool
	releaseSince     time.Time
	mutex            sync.Mutex
	callbacks        struct {
		onRotate func(direction int) // +1 for clockwise, -1 for counter-clockwise
		onClick  func()
		onHold   func() // Called after 3 second hold
	}
}

func NewEncoder() (*Encoder, error) {
	if simMode() {
		// No real GPIO on a dev machine; callbacks simply won't fire from
		// hardware input, but the rest of the app can still run.
		return &Encoder{position: 0}, nil
	}

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
			// Falling edge on A. Mechanical contact bounce on EC11-style
			// encoders can produce several spurious low/high transitions
			// within a couple of milliseconds of the real one - unlike the
			// push button and the three momentary buttons, this path had no
			// debounce at all, so a single physical detent could register
			// as two or more rotate events (or occasionally the wrong
			// direction, if B's level happened to change mid-bounce).
			if time.Since(e.lastRotationTime) >= 5*time.Millisecond {
				if currentB == gpio.Low {
					// B is also low, clockwise
					e.handleRotation(1)
				} else {
					// B is high, counter-clockwise
					e.handleRotation(-1)
				}
				e.lastRotationTime = time.Now()
			}
		}
	}

	e.lastA = currentA
	e.lastB = currentB
}

// buttonDebounce is the settling window applied to the encoder's push-switch.
// Unlike the rotation path (which debounces in readEncoder), the push button
// had no transition debounce: a single mid-hold bounce could make readButton
// take the release path mid-hold (firing a click while the user is still
// holding), then re-arm buttonDown/buttonTime when it bounced back - degrading
// a genuine 3s hold into a click. pressSince pins the time of the most recent
// *settled* edge so bounces within the window are ignored (a bit like the
// classic RC-debounced switch: the level must stay stable for buttonDebounce
// before it counts).
const buttonDebounce = 10 * time.Millisecond

func (e *Encoder) readButton() {
	currentButton := e.pinButton.Read() == gpio.Low // Active low

	e.mutex.Lock()
	defer e.mutex.Unlock()

	if currentButton && !e.buttonDown {
		// Button pressed. Only latch it once the line has been low (the
		// engaged state) continuously past the debounce window, so a bounce
		// that briefly releases it doesn't re-run the press logic.
		if !e.pressPending {
			e.pressPending = true
			e.pressSince = time.Now()
			return
		}
		if time.Since(e.pressSince) < buttonDebounce {
			return
		}
		e.pressPending = false
		e.buttonDown = true
		e.buttonTime = time.Now()
	} else if !currentButton && e.buttonDown {
		// Button released. Confirm the release held past the debounce window
		// before finishing the press, so bounce back down doesn't cancel a
		// just-completed click and re-arm a phantom hold.
		if !e.releasePending {
			e.releasePending = true
			e.releaseSince = time.Now()
			return
		}
		if time.Since(e.releaseSince) < buttonDebounce {
			return
		}
		e.releasePending = false
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
