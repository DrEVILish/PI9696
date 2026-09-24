package hardware

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"log/slog"

	"golang.org/x/image/font"
)

// FiraCodeManager handles FiraCode font integration for PI9696
type FiraCodeManager struct {
	display     *TTFDisplay
	config      *FiraCodeConfig
	currentFont string
	currentSize float64
	fontFaces   map[string]font.Face // cache keyed by "path@size", avoids re-parsing TTFs on every context switch
}

// FiraCodeConfig holds all FiraCode font variants and settings
type FiraCodeConfig struct {
	BasePath string
	Regular  string
	Bold     string
	Light    string
	Medium   string
	SemiBold string
	Retina   string
	sizes    map[string]float64
}

// NewFiraCodeManager creates a new FiraCode font manager
func NewFiraCodeManager() (*FiraCodeManager, error) {
	config := &FiraCodeConfig{
		BasePath: "./fonts",
		sizes: map[string]float64{
			"StatusBar":   9.0,  // Top status bar - compact but readable
			"MainContent": 11.0, // Primary content - optimal balance
			"MenuItems":   10.0, // Menu navigation - clean spacing
			"Headers":     13.0, // Section headers - prominent
			"Recording":   14.0, // Recording indicator - attention grabbing
			"Small":       8.0,  // Fine details - minimum readable
			"Large":       16.0, // Alerts/emphasis - maximum for display
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
		fontFaces:   make(map[string]font.Face),
	}
	manager.fontFaces[fontFaceKey(config.Regular, config.sizes["MainContent"])] = display.font

	slog.Info("FiraCode manager initialized successfully")
	return manager, nil
}

// fontFaceKey builds the cache key for a given font path and point size.
func fontFaceKey(fontPath string, fontSize float64) string {
	return fmt.Sprintf("%s@%.1f", fontPath, fontSize)
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
		slog.Warn(fmt.Sprintf("Optional FiraCode fonts not found: %v", missingOptional))
	}

	slog.Info(fmt.Sprintf("FiraCode fonts validated: Regular=%s, Bold=%s", fc.Regular, fc.Bold))
	return nil
}

// SwitchToContext changes font and size based on UI context. A missing
// optional face (Light/SemiBold/Medium installs vary) falls back to Regular
// at the requested size: without this a failed switch leaves the previous
// context's face stuck, since most draw callers ignore the error return.
func (fcm *FiraCodeManager) SwitchToContext(context string) error {
	fontPath := fcm.GetFontForContext(context)
	fontSize := fcm.GetSizeForContext(context)

	if fontPath == fcm.currentFont && fontSize == fcm.currentSize {
		return nil // Already using correct font/size
	}

	if err := fcm.switchFont(fontPath, fontSize); err != nil {
		if fontPath == fcm.config.Regular {
			return err
		}
		return fcm.switchFont(fcm.config.Regular, fontSize)
	}
	return nil
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
	case "recording", "alert":
		return fcm.config.sizes["Recording"]
	case "menu", "navigation", "settings", "selected", "active":
		return fcm.config.sizes["MenuItems"]
	case "header", "title", "section":
		return fcm.config.sizes["Headers"]
	case "details", "filename", "metadata":
		return fcm.config.sizes["Small"]
	case "emphasis", "large":
		return fcm.config.sizes["Large"]
	default:
		return fcm.config.sizes["MainContent"]
	}
}

// switchFont changes the active font face on the existing display. Faces are
// cached per (path, size) so repeated context switches (e.g. alternating
// "menu"/"selected" per menu item) don't re-read and re-parse TTF files or
// touch the SPI/GPIO connection - it only swaps which face draws text, so
// anything already drawn to the canvas this frame is preserved.
func (fcm *FiraCodeManager) switchFont(fontPath string, fontSize float64) error {
	key := fontFaceKey(fontPath, fontSize)

	face, ok := fcm.fontFaces[key]
	if !ok {
		var err error
		face, err = loadTTFFont(fontPath, fontSize)
		if err != nil {
			return fmt.Errorf("failed to load font %s at %.1fpt: %v", fontPath, fontSize, err)
		}
		fcm.fontFaces[key] = face
	}

	fcm.display.SetFontFace(face)
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
	return fcm.DrawStatusBarWithInferno(formatInfo, usbInfo, networkConnected, networkInfo, false)
}

// DrawStatusBarWithInferno renders the status bar with network, USB, and Inferno status
func (fcm *FiraCodeManager) DrawStatusBarWithInferno(formatInfo, usbInfo string, networkConnected bool, networkInfo string, infernoRunning bool) error {
	if err := fcm.SwitchToContext("statusbar"); err != nil {
		return err
	}

	// Determine USB connection status
	usbConnected := usbInfo != "" && usbInfo != "[---]" && usbInfo != "[ ]"

	// Use enhanced status bar with USB, network, and inferno icons
	fcm.display.DrawStatusBarWithIcons(formatInfo, usbInfo, usbConnected, networkConnected, networkInfo, infernoRunning)

	return nil
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

	return nil
}

// DrawRecordingStatus shows recording information with bold emphasis
func (fcm *FiraCodeManager) DrawRecordingStatus(elapsed, remaining, filename string) error {
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

	return nil
}

// DrawPlaybackStatus shows playback information: a title line with the play
// state and elapsed/total position, a progress bar showing the playhead as a
// relative offset, and the filename below. It mirrors DrawRecordingStatus's
// layout but adds the position readout (Round 3 seek/scrub) - a bar plus
// elapsed/total is how the operator sees where in the take they are.
func (fcm *FiraCodeManager) DrawPlaybackStatus(elapsed, total string, progress float64, filename string, paused bool) error {
	if err := fcm.SwitchToContext("recording"); err != nil {
		return err
	}
	state := "▶ PLAY"
	if paused {
		state = "⏸ PAUSED"
	}
	title := fmt.Sprintf("%s %s", state, elapsed)
	if total != "" {
		title = fmt.Sprintf("%s / %s", title, total)
	}
	fcm.display.DrawTextCentered(title, 24)

	// Progress bar: the playhead as a relative offset through the take.
	fcm.display.DrawProgressBar(0, 34, 256, 6, progress)

	if err := fcm.SwitchToContext("details"); err != nil {
		return err
	}
	if filename != "" {
		maxWidth := 256 - 32
		if fcm.display.GetTextWidth(filename) > maxWidth {
			avgCharWidth := fcm.display.GetTextWidth("M")
			maxChars := maxWidth/avgCharWidth - 3
			if maxChars > 0 && maxChars < len(filename) {
				filename = filename[:maxChars] + "..."
			}
		}
		fcm.display.DrawTextCentered(filename, 46)
	}

	return nil
}

// EncodePNG writes the current display frame as a PNG.
func (fcm *FiraCodeManager) EncodePNG(w io.Writer) error {
	return fcm.display.EncodePNG(w)
}

// DrawProgressBar renders a progress bar with percentage. details is the
// only text row below the bar - callers should fold any secondary hint
// (e.g. a cancel instruction) into that same string, since there isn't
// vertical room on a 64px display for a title, bar, percentage, and two
// more independent lines.
func (fcm *FiraCodeManager) DrawProgressBar(title string, progress float64, details string) error {
	// Title
	if err := fcm.SwitchToContext("header"); err != nil {
		return err
	}
	fcm.display.DrawTextCentered(title, 24)

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
	fcm.display.DrawTextCentered(percentText, 46)

	// Details
	if details != "" {
		fcm.display.DrawTextCentered(details, 58)
	}

	return nil
}

// DrawConfirmationDialog shows YES/NO confirmation with proper emphasis
func (fcm *FiraCodeManager) DrawConfirmationDialog(title, message1, message2 string, selectedOption int) error {
	// Fixed y positions sized for the "alert" (14pt) title and "menu" (10pt)
	// message fonts so nothing collides with the status bar above (rows
	// 0-11) or the YES/NO row below.
	const titleY, message1Y, message2Y, yesNoY = 24, 38, 49, 61

	// Title with emphasis
	if title != "" {
		if err := fcm.SwitchToContext("alert"); err != nil {
			return err
		}
		fcm.display.DrawTextCentered(title, titleY)
	}

	// Messages with regular font
	if err := fcm.SwitchToContext("menu"); err != nil {
		return err
	}

	if message1 != "" {
		fcm.display.DrawTextCentered(message1, message1Y)
	}

	if message2 != "" {
		fcm.display.DrawTextCentered(message2, message2Y)
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
		fcm.display.DrawText(96, yesNoY, yesText)

		if err := fcm.SwitchToContext("menu"); err != nil {
			return err
		}
		fcm.display.DrawText(160, yesNoY, noText)
	} else { // NO selected (default)
		if err := fcm.SwitchToContext("menu"); err != nil {
			return err
		}
		fcm.display.DrawText(96, yesNoY, yesText)

		if err := fcm.SwitchToContext("selected"); err != nil {
			return err
		}
		noText = "> NO"
		fcm.display.DrawText(160, yesNoY, noText)
	}

	return nil
}

// MenuItem represents a menu item with label and optional value
type MenuItem struct {
	Label string
	Value string
}

// Close releases resources used by the FiraCode manager
func (fcm *FiraCodeManager) Close() error {
	for _, face := range fcm.fontFaces {
		// display.font is seeded into the cache above: display.Close()
		// below owns it, closing it twice faults some face impls.
		if fcm.display != nil && face == fcm.display.font {
			continue
		}
		face.Close()
	}
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

// FrameHash passes through to the panel buffer checksum (see TTFDisplay);
// 0 when uninitialized so callers can distinguish "no display".
func (fcm *FiraCodeManager) FrameHash() uint64 {
	if fcm.display != nil {
		return fcm.display.FrameHash()
	}
	return 0
}

// CanvasHash passes through to the canvas checksum (see TTFDisplay);
// 0 when uninitialized.
func (fcm *FiraCodeManager) CanvasHash() uint64 {
	if fcm.display != nil {
		return fcm.display.CanvasHash()
	}
	return 0
}
