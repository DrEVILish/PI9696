package hardware

import (
	"fmt"

	"log/slog"

	"golang.org/x/image/font"
)

// HardwareManager bundles the front-panel devices. The display's drawing
// methods (FiraCodeManager, and through it TTFDisplay) are promoted.
type HardwareManager struct {
	*FiraCodeManager
	Encoder *Encoder
	Buttons *ButtonManager
	Network *NetworkDetector
	Lamps   *LampManager
}

func NewHardwareManager() (*HardwareManager, error) {
	hm := &HardwareManager{}

	// Initialize FiraCode display manager
	firacode, err := NewFiraCodeManager()
	if err != nil {
		// Fallback to basic display if FiraCode fails
		slog.Error(fmt.Sprintf("FiraCode initialization failed, attempting fallback: %v", err))

		// Try basic TTF display with system font
		basicDisplay, basicErr := NewTTFDisplay("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 11.0)
		if basicErr != nil {
			return nil, fmt.Errorf("failed to initialize any display: FiraCode=%v, Basic=%v", err, basicErr)
		}

		// Create a minimal FiraCode manager wrapper for the basic display
		firacode = &FiraCodeManager{
			TTFDisplay:  basicDisplay,
			faces:       fallbackFaces(),
			currentFont: "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			currentSize: 11.0,
			fontFaces:   make(map[string]font.Face),
		}
		firacode.fontFaces[fontFaceKey(firacode.currentFont, firacode.currentSize)] = basicDisplay.font
		slog.Warn("Using fallback display with system fonts")
	}
	hm.FiraCodeManager = firacode

	// Initialize network detector for eth0
	hm.Network = NewNetworkDetector("eth0")

	// Initialize encoder
	encoder, err := NewEncoder()
	if err != nil {
		hm.FiraCodeManager.Close()
		return nil, fmt.Errorf("failed to initialize encoder: %v", err)
	}
	hm.Encoder = encoder

	// Initialize buttons
	buttons, err := NewButtonManager()
	if err != nil {
		hm.Encoder.Close()
		hm.FiraCodeManager.Close()
		return nil, fmt.Errorf("failed to initialize buttons: %v", err)
	}
	hm.Buttons = buttons

	// Initialize button lamps (REC/PLAY backlights; STOP has no lamp).
	lamps, err := NewLampManager()
	if err != nil {
		hm.Encoder.Close()
		hm.Buttons.Close()
		hm.FiraCodeManager.Close()
		return nil, fmt.Errorf("failed to initialize lamps: %v", err)
	}
	hm.Lamps = lamps

	slog.Info("Hardware initialized successfully with FiraCode support")
	return hm, nil
}

func (hm *HardwareManager) Close() error {
	if hm.Encoder != nil {
		hm.Encoder.Close()
	}
	if hm.Buttons != nil {
		hm.Buttons.Close()
	}
	if hm.Lamps != nil {
		if err := hm.Lamps.Close(); err != nil {
			return err
		}
	}
	if hm.FiraCodeManager != nil {
		return hm.FiraCodeManager.Close()
	}
	return nil
}

// DrawStatusBarWithInferno draws the status bar including Inferno server status
func (hm *HardwareManager) DrawStatusBarWithInferno(formatInfo, usbInfo string, infernoRunning bool) error {
	networkConnected, networkInfo := hm.Network.GetNetworkStatus()
	return hm.FiraCodeManager.DrawStatusBarWithInferno(formatInfo, usbInfo, networkConnected, networkInfo, infernoRunning)
}

// FrameHash passes through to the panel buffer checksum for change-driven
// mirror refreshes; 0 when uninitialized.
func (hm *HardwareManager) FrameHash() uint64 {
	if hm != nil && hm.FiraCodeManager != nil && hm.TTFDisplay != nil {
		return hm.TTFDisplay.FrameHash()
	}
	return 0
}

// CanvasHash passes through to the supersampled-canvas checksum (see
// TTFDisplay.CanvasHash); 0 when uninitialized.
func (hm *HardwareManager) CanvasHash() uint64 {
	if hm != nil && hm.FiraCodeManager != nil && hm.TTFDisplay != nil {
		return hm.TTFDisplay.CanvasHash()
	}
	return 0
}

func (hm *HardwareManager) SetEncoderCallbacks(onRotate func(int), onClick func(), onHold func()) {
	if hm.Encoder != nil {
		hm.Encoder.SetRotateCallback(onRotate)
		hm.Encoder.SetClickCallback(onClick)
		hm.Encoder.SetHoldCallback(onHold)
	}
}

func (hm *HardwareManager) SetButtonCallback(buttonType ButtonType, callback func(ButtonType)) {
	if hm.Buttons != nil {
		hm.Buttons.SetCallback(buttonType, callback)
	}
}
