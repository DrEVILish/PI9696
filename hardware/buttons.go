package hardware

import (
	"fmt"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"sync"
	"time"
)

type ButtonType int

const (
	RecordButton ButtonType = iota
	StopButton
	PlayButton
)

type Button struct {
	pin        gpio.PinIn
	buttonType ButtonType
	pressed    bool
	lastChange time.Time
	mutex      sync.Mutex
	callback   func(ButtonType)
}

type ButtonManager struct {
	buttons []*Button
	quit    chan struct{}
	mutex   sync.Mutex
}

func NewButtonManager() (*ButtonManager, error) {
	bm := &ButtonManager{
		buttons: make([]*Button, 3),
		quit:    make(chan struct{}),
	}

	if simMode() {
		// No real GPIO on a dev machine; callbacks simply won't fire from
		// hardware input, but the rest of the app can still run.
		bm.buttons[RecordButton] = &Button{buttonType: RecordButton}
		bm.buttons[StopButton] = &Button{buttonType: StopButton}
		bm.buttons[PlayButton] = &Button{buttonType: PlayButton}
		return bm, nil
	}

	// Initialize Record button (GPIO5)
	recordPin := gpioreg.ByName("GPIO5")
	if recordPin == nil {
		return nil, fmt.Errorf("failed to get record button pin")
	}
	if err := recordPin.In(gpio.PullUp, gpio.BothEdges); err != nil {
		return nil, fmt.Errorf("failed to configure record button pin: %v", err)
	}
	bm.buttons[RecordButton] = &Button{
		pin:        recordPin,
		buttonType: RecordButton,
	}

	// Initialize Stop button (GPIO6)
	stopPin := gpioreg.ByName("GPIO6")
	if stopPin == nil {
		return nil, fmt.Errorf("failed to get stop button pin")
	}
	if err := stopPin.In(gpio.PullUp, gpio.BothEdges); err != nil {
		return nil, fmt.Errorf("failed to configure stop button pin: %v", err)
	}
	bm.buttons[StopButton] = &Button{
		pin:        stopPin,
		buttonType: StopButton,
	}

	// Initialize Play button (GPIO13)
	playPin := gpioreg.ByName("GPIO13")
	if playPin == nil {
		return nil, fmt.Errorf("failed to get play button pin")
	}
	if err := playPin.In(gpio.PullUp, gpio.BothEdges); err != nil {
		return nil, fmt.Errorf("failed to configure play button pin: %v", err)
	}
	bm.buttons[PlayButton] = &Button{
		pin:        playPin,
		buttonType: PlayButton,
	}

	// Start monitoring goroutine
	go bm.monitor()

	return bm, nil
}

func (bm *ButtonManager) monitor() {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-bm.quit:
			return
		case <-ticker.C:
			for _, button := range bm.buttons {
				bm.readButton(button)
			}
		}
	}
}

// Close stops the monitor goroutine.
func (bm *ButtonManager) Close() {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()
	select {
	case <-bm.quit:
	default:
		close(bm.quit)
	}
}

func (bm *ButtonManager) readButton(button *Button) {
	currentState := button.pin.Read() == gpio.Low // Active low (pressed when low)

	button.mutex.Lock()
	defer button.mutex.Unlock()

	// Debounce both transitions, not just press. The previous version only
	// guarded the press edge; the release edge cleared `pressed` on the
	// first low->high reading with no debounce at all. A single bounce
	// while the button was still physically held would clear `pressed`,
	// and once the original press's debounce window elapsed the next low
	// reading (the same continuous press) was treated as a brand-new press
	// and fired the callback a second time.
	now := time.Now()
	if now.Sub(button.lastChange) < 50*time.Millisecond {
		return
	}

	if currentState && !button.pressed {
		// Button just pressed
		button.pressed = true
		button.lastChange = now

		if button.callback != nil {
			go button.callback(button.buttonType)
		}
	} else if !currentState && button.pressed {
		// Button released
		button.pressed = false
		button.lastChange = now
	}
}

func (bm *ButtonManager) SetCallback(buttonType ButtonType, callback func(ButtonType)) {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()

	if int(buttonType) < len(bm.buttons) && bm.buttons[buttonType] != nil {
		bm.buttons[buttonType].mutex.Lock()
		bm.buttons[buttonType].callback = callback
		bm.buttons[buttonType].mutex.Unlock()
	}
}

func (bm *ButtonManager) IsPressed(buttonType ButtonType) bool {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()

	if int(buttonType) < len(bm.buttons) && bm.buttons[buttonType] != nil {
		bm.buttons[buttonType].mutex.Lock()
		pressed := bm.buttons[buttonType].pressed
		bm.buttons[buttonType].mutex.Unlock()
		return pressed
	}
	return false
}

func (bt ButtonType) String() string {
	switch bt {
	case RecordButton:
		return "Record"
	case StopButton:
		return "Stop"
	case PlayButton:
		return "Play"
	default:
		return "Unknown"
	}
}
