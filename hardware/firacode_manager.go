package hardware

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// FiraCodeManager handles FiraCode font integration for PI9696
type FiraCodeManager struct {
	display     *TTFDisplay
	config      *FiraCodeConfig
	currentFont string
	currentSize float64
}

// FiraCodeConfig holds all FiraCode font variants and settings
type FiraCodeConfig struct {
	BasePath   string
	Regular    string
	Bold       string
	Light      string
	Medium     string
	SemiBold   string
	Retina     string
	sizes      map[string]float64
}

// NewFiraCodeManager creates a new FiraCode font manager
func NewFiraCodeManager() (*FiraCodeManager, error) {
	config := &FiraCodeConfig{
		BasePath: "./fonts",
		sizes: map[string]float64{
			"StatusBar":    9.0,  // Top status bar - compact but readable
			"MainContent":  11.0, // Primary content - optimal balance
			"MenuItems":    10.0, // Menu navigation - clean spacing
			"Headers":      13.0, // Section headers - prominent
			"Recording":    14.0, // Recording indicator - attention grabbing
			"Small":        8.0,  // Fine details - minimum readable
			"Large":        16.0, // Alerts/emphasis - maximum for display
		},
	}

	// Set font paths
	config.Regular = filepath.Join(config.BasePath, "FiraCode-Regular.ttf")
	config.Bold = filepath.Join(config.BasePath, "FiraCode-Bold.ttf")
	config.Light = filepath.Join(config.BasePath, "FiraCode-Light.ttf")
	config.Medium = filepath.Join(config.BasePath, "FiraCode-Medium.ttf")
	config.SemiBold = filepath.Join(config.BasePath, "FiraCode-SemiBold.ttf")
	config.Retina = filepath.Join(config.BasePath, "FiraCode-Retina.ttf")

	// Validate installation
	if err := config.ValidateInstallation(); err != nil {
		return nil, fmt.Errorf("FiraCode validation failed: %v", err)
	}

	// Initialize with regular font at main content size
	display, err := NewTTFDisplay(config.Regular, config.sizes["MainContent"])
	if err != nil {
		return nil, fmt.Errorf("failed to initialize FiraCode display: %v", err)
	}

	manager := &FiraCodeManager{
		display:     display,
		config:      config,
		currentFont: config.Regular,
		currentSize: config.sizes["MainContent"],
	}

	log.Printf("FiraCode manager initialized successfully")
	return manager, nil
}

// ValidateInstallation checks if required FiraCode fonts are available
func (fc *FiraCodeConfig) ValidateInstallation() error {
	requiredFonts := map[string]string{
		"Regular": fc.Regular,
		"Bold":    fc.Bold,
	}

	optionalFonts := map[string]string{
		"Light":    fc.Light,
		"Medium":   fc.Medium,
		"SemiBold": fc.SemiBold,
		"Retina":   fc.Retina,
	}

	var missingRequired []string
	var missingOptional []string

	// Check required fonts
	for name, path := range requiredFonts {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missingRequired = append(missingRequired, fmt.Sprintf("%s (%s)", name, path))
		}
	}

	// Check optional fonts
	for name, path := range optionalFonts {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missingOptional = append(missingOptional, name)
		}
	}

	if len(missingRequired) > 0 {
		return fmt.Errorf("missing required FiraCode fonts: %v", missingRequired)
	}

	if len(missingOptional) > 0 {
		log.Printf("Optional FiraCode fonts not found: %v", missingOptional)
	}

	log.Printf("FiraCode fonts validated: Regular=%s, Bold=%s", fc.Regular, fc.Bold)
	return nil
}

// SwitchToContext changes font and size based on UI context
func (fcm *FiraCodeManager) SwitchToContext(context string) error {
	fontPath := fcm.GetFontForContext(context)
	fontSize := fcm.GetSizeForContext(context)

	if fontPath == fcm.currentFont && fontSize == fcm.currentSize {
		return nil // Already using correct font/size
	}

	return fcm.switchFont(fontPath, fontSize)
}

// GetFontForContext returns the best font variant for different UI contexts
func (fcm *FiraCodeManager) GetFontForContext(context string) string {
	switch context {
	case "statusbar", "time", "counters", "storage":
		return fcm.config.Regular
	case "recording", "alert", "error", "warning":
		return fcm.config.Bold
	case "menu", "navigation", "settings":
		return fcm.config.Regular
	case "details", "filename", "path", "metadata":
		return fcm.config.Light
	case "emphasis", "selected", "active":
		return fcm.config.SemiBold
	case "header", "title", "section":
		return fcm.config.Medium
	case "standby", "idle":
		return fcm.config.Regular
	default:
		return fcm.config.Regular
	}
}

// GetSizeForContext returns optimal font size for different UI contexts
func (fcm *FiraCodeManager) GetSizeForContext(context string) float64 {
	switch context {
	case "statusbar":
		return fcm.config.sizes["StatusBar"]
	case "recording", "alert", "header":
		return fcm.config.sizes["Recording"]
	case "menu", "navigation", "settings":
		return fcm.config.sizes["MenuItems"]
	case "title", "section":
		return fcm.config.sizes["Headers"]
	case "details", "filename", "metadata":
		return fcm.config.sizes["Small"]
	case "emphasis", "large":
		return fcm.config.sizes["Large"]
	default:
		return fcm.config.sizes["MainContent"]
	}
}

// switchFont changes the current font and size
func (fcm *FiraCodeManager) switchFont(fontPath string, fontSize float64) error {
	// Close current display
	if fcm.display != nil {
		fcm.display.Close()
	}

	// Create new display with specified font
	newDisplay, err := NewTTFDisplay(fontPath, fontSize)
	if err != nil {
		// Try to restore previous font
		fcm.display, _ = NewTTFDisplay(fcm.currentFont, fcm.currentSize)
		return fmt.Errorf("failed to switch to font %s at %.1fpt: %v", fontPath, fontSize, err)
	}

	fcm.display = newDisplay
	fcm.currentFont = fontPath
	fcm.currentSize = fontSize

	return nil
}

// Display utility methods for different UI contexts

// DrawStatusBar renders the top status bar with appropriate FiraCode styling
func (fcm *FiraCodeManager) DrawStatusBar(formatInfo, usbInfo string) error {
	return fcm.DrawStatusBarWithNetwork(formatInfo, usbInfo, false, "")
}

// DrawStatusBarWithNetwork renders the status bar with network and USB status
func (fcm *FiraCodeManager) DrawStatusBarWithNetwork(formatInfo, usbInfo string, networkConnected bool, networkInfo string) error {
	if err := fcm.SwitchToContext("statusbar"); err != nil {
		return err
	}

	fcm.display.Clear()

	// Determine USB connection status
	usbConnected := usbInfo != "" && usbInfo != "[---]" && usbInfo != "[ ]"
	
	// Use enhanced status bar with both USB and network icons
	fcm.display.DrawStatusBarWithIcons(formatInfo, usbInfo, usbConnected, networkConnected, networkInfo)

	return fcm.display.Update()
}

// DrawCenteredText draws text centered with context-appropriate styling
func (fcm *FiraCodeManager) DrawCenteredText(text, context string, y int) error {
	if err := fcm.SwitchToContext(context); err != nil {
		return err
	}

	fcm.display.DrawTextCentered(text, y)
	return nil
}

// DrawMenuItems renders menu items with proper font weights
func (fcm *FiraCodeManager) DrawMenuItems(items []MenuItem, selectedIndex int) error {
	if err := fcm.SwitchToContext("menu"); err != nil {
		return err
	}

	y := 24 // Start below status bar
	fontHeight := fcm.display.GetFontHeight()

	for i, item := range items {
		// Switch to emphasis font for selected items
		if i == selectedIndex {
			if err := fcm.SwitchToContext("selected"); err != nil {
				return err
			}
		} else {
			if err := fcm.SwitchToContext("menu"); err != nil {
				return err
			}
		}

		prefix := "  "
		if i == selectedIndex {
			prefix = "> "
		}

		// Draw label
		labelText := prefix + item.Label
		fcm.display.DrawText(8, y, labelText)

		// Draw right-aligned value if present
		if item.Value != "" {
			valueWidth := fcm.display.GetTextWidth(item.Value)
			fcm.display.DrawText(256-valueWidth-16, y, item.Value)
		}

		y += fontHeight + 2

		// Don't draw beyond display bounds
		if y >= 64-fontHeight {
			break
		}
	}

	return fcm.display.Update()
}

// DrawRecordingStatus shows recording information with bold emphasis
func (fcm *FiraCodeManager) DrawRecordingStatus(elapsed, remaining, filename string) error {
	fcm.display.Clear()

	// Recording indicator with bold font
	if err := fcm.SwitchToContext("recording"); err != nil {
		return err
	}
	recText := fmt.Sprintf("● REC %s", elapsed)
	fcm.display.DrawTextCentered(recText, 24)

	// Time remaining with regular font
	if err := fcm.SwitchToContext("details"); err != nil {
		return err
	}
	timeText := fmt.Sprintf("Time Remaining: %s", remaining)
	fcm.display.DrawTextCentered(timeText, 40)

	// Filename with light font
	if filename != "" {
		// Truncate filename if too long
		maxWidth := 256 - 32 // Leave margins
		if fcm.display.GetTextWidth(filename) > maxWidth {
			// Estimate characters that fit
			avgCharWidth := fcm.display.GetTextWidth("M") // Use 'M' as average width
			maxChars := maxWidth/avgCharWidth - 3         // Reserve space for "..."
			if maxChars > 0 && maxChars < len(filename) {
				filename = filename[:maxChars] + "..."
			}
		}
		fcm.display.DrawTextCentered(filename, 56)
	}

	return fcm.display.Update()
}

// DrawProgressBar renders a progress bar with percentage
func (fcm *FiraCodeManager) DrawProgressBar(title string, progress float64, details string) error {
	fcm.display.Clear()

	// Title
	if err := fcm.SwitchToContext("header"); err != nil {
		return err
	}
	fcm.display.DrawTextCentered(title, 20)

	// Progress bar (32 characters wide, centered)
	barWidth := 32
	barX := (256 - barWidth*8) / 2
	barY := 32
	fcm.display.DrawProgressBar(barX, barY, barWidth*8, 8, progress/100.0)

	// Percentage text
	if err := fcm.SwitchToContext("details"); err != nil {
		return err
	}
	percentText := fmt.Sprintf("%.0f%%", progress)
	fcm.display.DrawTextCentered(percentText, 48)

	// Details
	if details != "" {
		fcm.display.DrawTextCentered(details, 56)
	}

	return fcm.display.Update()
}

// DrawConfirmationDialog shows YES/NO confirmation with proper emphasis
func (fcm *FiraCodeManager) DrawConfirmationDialog(title, message1, message2 string, selectedOption int) error {
	fcm.display.Clear()

	y := 16

	// Title with emphasis
	if title != "" {
		if err := fcm.SwitchToContext("alert"); err != nil {
			return err
		}
		fcm.display.DrawTextCentered(title, y)
		y += 16
	}

	// Messages with regular font
	if err := fcm.SwitchToContext("menu"); err != nil {
		return err
	}

	if message1 != "" {
		fcm.display.DrawTextCentered(message1, y)
		y += 12
	}

	if message2 != "" {
		fcm.display.DrawTextCentered(message2, y)
	}

	// YES/NO options
	yesText := "YES"
	noText := "NO"

	// Emphasize selected option
	if selectedOption == 1 { // YES selected
		if err := fcm.SwitchToContext("selected"); err != nil {
			return err
		}
		yesText = "> YES"
		fcm.display.DrawText(96, 56, yesText)

		if err := fcm.SwitchToContext("menu"); err != nil {
			return err
		}
		fcm.display.DrawText(160, 56, noText)
	} else { // NO selected (default)
		if err := fcm.SwitchToContext("menu"); err != nil {
			return err
		}
		fcm.display.DrawText(96, 56, yesText)

		if err := fcm.SwitchToContext("selected"); err != nil {
			return err
		}
		noText = "> NO"
		fcm.display.DrawText(160, 56, noText)
	}

	return fcm.display.Update()
}

// MenuItem represents a menu item with label and optional value
type MenuItem struct {
	Label string
	Value string
}

// GetDisplay returns the underlying TTF display for direct access
func (fcm *FiraCodeManager) GetDisplay() *TTFDisplay {
	return fcm.display
}

// GetCurrentFont returns the currently active font path
func (fcm *FiraCodeManager) GetCurrentFont() string {
	return fcm.currentFont
}

// GetCurrentSize returns the currently active font size
func (fcm *FiraCodeManager) GetCurrentSize() float64 {
	return fcm.currentSize
}

// GetAvailableFonts returns a list of available FiraCode variants
func (fcm *FiraCodeManager) GetAvailableFonts() map[string]string {
	fonts := make(map[string]string)
	
	variants := map[string]string{
		"Regular":  fcm.config.Regular,
		"Bold":     fcm.config.Bold,
		"Light":    fcm.config.Light,
		"Medium":   fcm.config.Medium,
		"SemiBold": fcm.config.SemiBold,
		"Retina":   fcm.config.Retina,
	}

	// Only include fonts that exist
	for name, path := range variants {
		if _, err := os.Stat(path); err == nil {
			fonts[name] = path
		}
	}

	return fonts
}

// Close releases resources used by the FiraCode manager
func (fcm *FiraCodeManager) Close() error {
	if fcm.display != nil {
		return fcm.display.Close()
	}
	return nil
}

// ClearDisplay clears the display buffer
func (fcm *FiraCodeManager) ClearDisplay() {
	if fcm.display != nil {
		fcm.display.Clear()
	}
}

// UpdateDisplay sends the current buffer to the physical display
func (fcm *FiraCodeManager) UpdateDisplay() error {
	if fcm.display != nil {
		return fcm.display.Update()
	}
	return fmt.Errorf("display not initialized")
}