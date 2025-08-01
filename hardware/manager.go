package hardware

import (
	"fmt"
	"log"
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
		log.Printf("FiraCode initialization failed, attempting fallback: %v", err)
		
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
		}
		log.Println("Using fallback display with system fonts")
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

	log.Println("Hardware initialized successfully with FiraCode support")
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

func (hm *HardwareManager) DrawCenteredText(text, context string, y int) error {
	return hm.FiraCode.DrawCenteredText(text, context, y)
}

func (hm *HardwareManager) DrawMenuItems(items []MenuItem, selectedIndex int) error {
	return hm.FiraCode.DrawMenuItems(items, selectedIndex)
}

func (hm *HardwareManager) DrawRecordingStatus(elapsed, remaining, filename string) error {
	return hm.FiraCode.DrawRecordingStatus(elapsed, remaining, filename)
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

// Encoder utility methods

func (hm *HardwareManager) SetEncoderCallbacks(onRotate func(int), onClick func(), onHold func()) {
	if hm.Encoder != nil {
		hm.Encoder.SetRotateCallback(onRotate)
		hm.Encoder.SetClickCallback(onClick)
		hm.Encoder.SetHoldCallback(onHold)
	}
}

func (hm *HardwareManager) GetEncoderPosition() int {
	if hm.Encoder != nil {
		return hm.Encoder.GetPosition()
	}
	return 0
}

func (hm *HardwareManager) ResetEncoderPosition() {
	if hm.Encoder != nil {
		hm.Encoder.ResetPosition()
	}
}

func (hm *HardwareManager) IsEncoderPressed() bool {
	if hm.Encoder != nil {
		return hm.Encoder.IsButtonPressed()
	}
	return false
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

func (hm *HardwareManager) GetCurrentFont() string {
	if hm.FiraCode != nil {
		return hm.FiraCode.GetCurrentFont()
	}
	return ""
}

func (hm *HardwareManager) GetCurrentSize() float64 {
	if hm.FiraCode != nil {
		return hm.FiraCode.GetCurrentSize()
	}
	return 0.0
}

func (hm *HardwareManager) GetAvailableFonts() map[string]string {
	if hm.FiraCode != nil {
		return hm.FiraCode.GetAvailableFonts()
	}
	return make(map[string]string)
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

// Display information methods

func (hm *HardwareManager) GetDisplayWidth() int {
	return DisplayWidth
}

func (hm *HardwareManager) GetDisplayHeight() int {
	return DisplayHeight
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

// Hardware status methods

func (hm *HardwareManager) GetHardwareStatus() map[string]interface{} {
	status := make(map[string]interface{})
	
	// Display status
	if hm.FiraCode != nil {
		status["display"] = map[string]interface{}{
			"type":         "FiraCode TTF",
			"current_font": hm.FiraCode.GetCurrentFont(),
			"current_size": hm.FiraCode.GetCurrentSize(),
			"available_fonts": len(hm.FiraCode.GetAvailableFonts()),
		}
	} else {
		status["display"] = "not initialized"
	}
	
	// Encoder status
	if hm.Encoder != nil {
		status["encoder"] = map[string]interface{}{
			"position": hm.Encoder.GetPosition(),
			"pressed":  hm.Encoder.IsButtonPressed(),
		}
	} else {
		status["encoder"] = "not initialized"
	}
	
	// Button status
	if hm.Buttons != nil {
		status["buttons"] = map[string]interface{}{
			"record": hm.Buttons.IsPressed(RecordButton),
			"stop":   hm.Buttons.IsPressed(StopButton),
			"play":   hm.Buttons.IsPressed(PlayButton),
		}
	} else {
		status["buttons"] = "not initialized"
	}
	
	// Network status
	if hm.Network != nil {
		networkInfo, _ := hm.Network.GetNetworkInfo()
		status["network"] = map[string]interface{}{
			"interface":  networkInfo.InterfaceName,
			"connected":  networkInfo.Connected,
			"ip_address": networkInfo.IPAddress,
			"link_up":    networkInfo.LinkUp,
		}
	} else {
		status["network"] = "not initialized"
	}
	
	return status
}

// Test methods for hardware validation

func (hm *HardwareManager) TestDisplay() error {
	if hm.FiraCode == nil {
		return fmt.Errorf("FiraCode manager not initialized")
	}
	
	// Test different contexts and fonts
	contexts := []string{"statusbar", "header", "recording", "menu", "details"}
	
	for i, context := range contexts {
		hm.ClearDisplay()
		
		testText := fmt.Sprintf("Test %s", context)
		y := 16 + i*10
		
		if err := hm.DrawCenteredText(testText, context, y); err != nil {
			return fmt.Errorf("failed to draw text in context %s: %v", context, err)
		}
		
		if err := hm.UpdateDisplay(); err != nil {
			return fmt.Errorf("failed to update display: %v", err)
		}
	}
	
	return nil
}

func (hm *HardwareManager) TestEncoder() error {
	if hm.Encoder == nil {
		return fmt.Errorf("encoder not initialized")
	}
	
	// Test encoder position
	initialPos := hm.Encoder.GetPosition()
	hm.Encoder.ResetPosition()
	
	if hm.Encoder.GetPosition() != 0 {
		return fmt.Errorf("encoder reset failed")
	}
	
	log.Printf("Encoder test passed - initial position: %d", initialPos)
	return nil
}

func (hm *HardwareManager) TestButtons() error {
	if hm.Buttons == nil {
		return fmt.Errorf("buttons not initialized")
	}
	
	// Test each button
	buttons := []ButtonType{RecordButton, StopButton, PlayButton}
	
	for _, button := range buttons {
		pressed := hm.Buttons.IsPressed(button)
		log.Printf("Button %s: pressed=%v", button.String(), pressed)
	}
	
	return nil
}

func (hm *HardwareManager) TestAll() error {
	log.Println("Testing all hardware components...")
	
	if err := hm.TestDisplay(); err != nil {
		return fmt.Errorf("display test failed: %v", err)
	}
	log.Println("✓ Display test passed")
	
	if err := hm.TestEncoder(); err != nil {
		return fmt.Errorf("encoder test failed: %v", err)
	}
	log.Println("✓ Encoder test passed")
	
	if err := hm.TestButtons(); err != nil {
		return fmt.Errorf("buttons test failed: %v", err)
	}
	log.Println("✓ Buttons test passed")
	
	log.Println("All hardware tests completed successfully")
	return nil
}