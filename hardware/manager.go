package hardware

import (
	"fmt"
	"io"
	"time"

	"pi9696/xlog"

	"golang.org/x/image/font"
)

type HardwareManager struct {
	FiraCode *FiraCodeManager
	Encoder  *Encoder
	Buttons  *ButtonManager
	Network  *NetworkDetector
}

func NewHardwareManager() (*HardwareManager, error) {
	hm := &HardwareManager{}

	// Initialize FiraCode display manager
	firacode, err := NewFiraCodeManager()
	if err != nil {
		// Fallback to basic display if FiraCode fails
		xlog.Errorf("FiraCode initialization failed, attempting fallback: %v", err)

		// Try basic TTF display with system font
		basicDisplay, basicErr := NewTTFDisplay("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 11.0)
		if basicErr != nil {
			return nil, fmt.Errorf("failed to initialize any display: FiraCode=%v, Basic=%v", err, basicErr)
		}

		// Create a minimal FiraCode manager wrapper for the basic display
		firacode = &FiraCodeManager{
			display: basicDisplay,
			config: &FiraCodeConfig{
				BasePath: "./fonts",
				Regular:  "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
				Bold:     "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
			},
			currentFont: "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			currentSize: 11.0,
			fontFaces:   make(map[string]font.Face),
		}
		firacode.fontFaces[fontFaceKey(firacode.currentFont, firacode.currentSize)] = basicDisplay.font
		xlog.Warnf("Using fallback display with system fonts")
	}
	hm.FiraCode = firacode

	// Initialize network detector for eth0
	hm.Network = NewNetworkDetector("eth0")

	// Initialize encoder
	encoder, err := NewEncoder()
	if err != nil {
		hm.FiraCode.Close()
		return nil, fmt.Errorf("failed to initialize encoder: %v", err)
	}
	hm.Encoder = encoder

	// Initialize buttons
	buttons, err := NewButtonManager()
	if err != nil {
		hm.FiraCode.Close()
		return nil, fmt.Errorf("failed to initialize buttons: %v", err)
	}
	hm.Buttons = buttons

	xlog.Infof("Hardware initialized successfully with FiraCode support")
	return hm, nil
}

func (hm *HardwareManager) Close() error {
	if hm.FiraCode != nil {
		return hm.FiraCode.Close()
	}
	return nil
}

// Display utility methods using FiraCode manager

func (hm *HardwareManager) ClearDisplay() {
	if hm.FiraCode != nil {
		hm.FiraCode.ClearDisplay()
	}
}

func (hm *HardwareManager) UpdateDisplay() error {
	if hm.FiraCode != nil {
		return hm.FiraCode.UpdateDisplay()
	}
	return nil
}

// Context-aware text drawing methods

func (hm *HardwareManager) DrawStatusBar(formatInfo, usbInfo string) error {
	// Get network status
	networkConnected, networkInfo := hm.Network.GetNetworkStatus()
	return hm.FiraCode.DrawStatusBarWithNetwork(formatInfo, usbInfo, networkConnected, networkInfo)
}

// DrawStatusBarWithInferno draws the status bar including Inferno server status
func (hm *HardwareManager) DrawStatusBarWithInferno(formatInfo, usbInfo string, infernoRunning bool) error {
	// Get network status
	networkConnected, networkInfo := hm.Network.GetNetworkStatus()
	return hm.FiraCode.DrawStatusBarWithInferno(formatInfo, usbInfo, networkConnected, networkInfo, infernoRunning)
}

func (hm *HardwareManager) DrawCenteredText(text, context string, y int) error {
	return hm.FiraCode.DrawCenteredText(text, context, y)
}

func (hm *HardwareManager) DrawMenuItems(items []MenuItem, selectedIndex int) error {
	return hm.FiraCode.DrawMenuItems(items, selectedIndex)
}

func (hm *HardwareManager) DrawRecordingStatus(elapsed, remaining, filename string) error {
	return hm.FiraCode.DrawRecordingStatus(elapsed, remaining, filename)
}

func (hm *HardwareManager) EncodePNG(w io.Writer) error {
	return hm.FiraCode.EncodePNG(w)
}

func (hm *HardwareManager) DrawPlaybackStatus(elapsed, total time.Duration, filename string, paused bool) error {
	return hm.FiraCode.DrawPlaybackStatus(elapsed, total, filename, paused)
}

func (hm *HardwareManager) DrawProgressBar(title string, progress float64, details string) error {
	return hm.FiraCode.DrawProgressBar(title, progress, details)
}

func (hm *HardwareManager) DrawConfirmationDialog(title, message1, message2 string, selectedOption int) error {
	return hm.FiraCode.DrawConfirmationDialog(title, message1, message2, selectedOption)
}

// Legacy compatibility methods for existing code

func (hm *HardwareManager) DrawText(x, y int, text string) {
	if hm.FiraCode != nil && hm.FiraCode.display != nil {
		hm.FiraCode.display.DrawText(x, y, text)
	}
}

func (hm *HardwareManager) SetPixel(x, y int, brightness byte) {
	if hm.FiraCode != nil && hm.FiraCode.display != nil {
		hm.FiraCode.display.SetPixel(x, y, brightness)
	}
}

// SetBrightness forwards a 0-100% panel brightness to the physical display;
// also used by auto-dim (dim level, then 0 for off) and by wake-on-input to
// restore the user's level. No-op in sim mode (writeCommand is).
func (hm *HardwareManager) SetBrightness(pct int) {
	if hm.FiraCode != nil && hm.FiraCode.display != nil {
		hm.FiraCode.display.SetBrightness(pct)
	}
}

func (hm *HardwareManager) DrawBox(x, y, width, height int, brightness byte) {
	if hm.FiraCode != nil && hm.FiraCode.display != nil {
		hm.FiraCode.display.DrawBox(x, y, width, height, brightness)
	}
}

func (hm *HardwareManager) FillBox(x, y, width, height int, brightness byte) {
	if hm.FiraCode != nil && hm.FiraCode.display != nil {
		hm.FiraCode.display.FillBox(x, y, width, height, brightness)
	}
}

// Encoder utility methods

func (hm *HardwareManager) SetEncoderCallbacks(onRotate func(int), onClick func(), onHold func()) {
	if hm.Encoder != nil {
		hm.Encoder.SetRotateCallback(onRotate)
		hm.Encoder.SetClickCallback(onClick)
		hm.Encoder.SetHoldCallback(onHold)
	}
}

// Button utility methods

func (hm *HardwareManager) SetButtonCallback(buttonType ButtonType, callback func(ButtonType)) {
	if hm.Buttons != nil {
		hm.Buttons.SetCallback(buttonType, callback)
	}
}

func (hm *HardwareManager) IsButtonPressed(buttonType ButtonType) bool {
	if hm.Buttons != nil {
		return hm.Buttons.IsPressed(buttonType)
	}
	return false
}

// Font management methods

func (hm *HardwareManager) SwitchToContext(context string) error {
	if hm.FiraCode != nil {
		return hm.FiraCode.SwitchToContext(context)
	}
	return nil
}

// Network utility methods

func (hm *HardwareManager) GetNetworkInfo() (*NetworkInfo, error) {
	if hm.Network != nil {
		return hm.Network.GetNetworkInfo()
	}
	return nil, fmt.Errorf("network detector not initialized")
}

func (hm *HardwareManager) GetNetworkStatus() (bool, string) {
	if hm.Network != nil {
		return hm.Network.GetNetworkStatus()
	}
	return false, "No Network"
}

func (hm *HardwareManager) GetDetailedNetworkInfo() []string {
	if hm.Network != nil {
		return hm.Network.GetDetailedNetworkInfo()
	}
	return []string{"Network Error", "Not initialized"}
}

func (hm *HardwareManager) IsNetworkAvailable() bool {
	if hm.Network != nil {
		return hm.Network.IsNetworkAvailable()
	}
	return false
}

func (hm *HardwareManager) GetFontHeight() int {
	if hm.FiraCode != nil && hm.FiraCode.display != nil {
		return hm.FiraCode.display.GetFontHeight()
	}
	return 12 // Default fallback
}

func (hm *HardwareManager) GetTextWidth(text string) int {
	if hm.FiraCode != nil && hm.FiraCode.display != nil {
		return hm.FiraCode.display.GetTextWidth(text)
	}
	return len(text) * 8 // Fallback estimation
}
