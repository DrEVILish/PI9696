package hardware

import (
	"fmt"
	"os"
	"path/filepath"

	"log/slog"

	"golang.org/x/image/font"
)

// FiraCodeManager handles FiraCode font integration for PI9696
type FiraCodeManager struct {
	*TTFDisplay // its drawing primitives are used directly
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
// defaultFontSizes are the per-context point sizes. Shared with the system-
// font fallback in NewHardwareManager: a config without them makes every
// SwitchToContext load a 0pt face, after which nothing renders at all.
func defaultFontSizes() map[string]float64 {
	return map[string]float64{
		"StatusBar":   9.0,  // Top status bar - compact but readable
		"MainContent": 11.0, // Primary content - optimal balance
		"MenuItems":   10.0, // Menu navigation - clean spacing
		"Headers":     13.0, // Section headers - prominent
		"Recording":   14.0, // Recording indicator - attention grabbing
		"Small":       8.0,  // Fine details - minimum readable
		"Large":       16.0, // Alerts/emphasis - maximum for display
	}
}

// fallbackFiraCodeConfig is the system-font (DejaVu) config used when the
// FiraCode files cannot be loaded.
func fallbackFiraCodeConfig() *FiraCodeConfig {
	return &FiraCodeConfig{
		BasePath: "./fonts",
		Regular:  "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		Bold:     "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
		sizes:    defaultFontSizes(),
	}
}

func NewFiraCodeManager() (*FiraCodeManager, error) {
	config := &FiraCodeConfig{
		BasePath: "./fonts",
		sizes:    defaultFontSizes(),
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
		TTFDisplay:  display,
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

	fcm.TTFDisplay.SetFontFace(face)
	fcm.currentFont = fontPath
	fcm.currentSize = fontSize

	return nil
}

// Display utility methods for different UI contexts

// DrawStatusBarWithInferno renders the status bar with network, USB, and Inferno status
func (fcm *FiraCodeManager) DrawStatusBarWithInferno(formatInfo, usbInfo string, networkConnected bool, networkInfo string, infernoRunning bool) error {
	if err := fcm.SwitchToContext("statusbar"); err != nil {
		return err
	}

	// Determine USB connection status
	usbConnected := usbInfo != "" && usbInfo != "[---]" && usbInfo != "[ ]"

	// Use enhanced status bar with USB, network, and inferno icons
	fcm.TTFDisplay.DrawStatusBarWithIcons(formatInfo, usbInfo, usbConnected, networkConnected, networkInfo, infernoRunning)

	return nil
}

// DrawCenteredText draws text centered with context-appropriate styling
func (fcm *FiraCodeManager) DrawCenteredText(text, context string, y int) error {
	if err := fcm.SwitchToContext(context); err != nil {
		return err
	}

	fcm.TTFDisplay.DrawTextCentered(text, y)
	return nil
}

// truncateRunes shortens s to at most maxChars runes, appending "..." when
// shortened. Byte slicing would split multi-byte UTF-8 mid-sequence; kept as
// a helper (rather than inline) so the boundary is unit-testable without a
// display. A non-positive budget returns s unchanged, matching the old
// behavior of skipping truncation entirely in that degenerate case.
func truncateRunes(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + "..."
}

// DrawRecordingStatus shows recording information with bold emphasis
func (fcm *FiraCodeManager) DrawRecordingStatus(elapsed, remaining, filename string) error {
	// Recording indicator with bold font
	if err := fcm.SwitchToContext("recording"); err != nil {
		return err
	}
	recText := fmt.Sprintf("● REC %s", elapsed)
	fcm.TTFDisplay.DrawTextCentered(recText, 24)

	// Time remaining with regular font
	if err := fcm.SwitchToContext("details"); err != nil {
		return err
	}
	timeText := fmt.Sprintf("Time Remaining: %s", remaining)
	fcm.TTFDisplay.DrawTextCentered(timeText, 40)

	// Filename with light font
	if filename != "" {
		// Truncate filename if too long, rune-wise: maxChars counts
		// characters but len(filename) counts bytes, so byte slicing would
		// split multi-byte UTF-8 mid-sequence and render as garbage.
		maxWidth := 256 - 32 // Leave margins
		if fcm.TTFDisplay.GetTextWidth(filename) > maxWidth {
			// Estimate characters that fit
			avgCharWidth := fcm.TTFDisplay.GetTextWidth("M") // Use 'M' as average width
			maxChars := maxWidth/avgCharWidth - 3            // Reserve space for "..."
			filename = truncateRunes(filename, maxChars)
		}
		fcm.TTFDisplay.DrawTextCentered(filename, 56)
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
	fcm.TTFDisplay.DrawTextCentered(title, 24)

	// Progress bar: the playhead as a relative offset through the take.
	fcm.TTFDisplay.DrawProgressBar(0, 34, 256, 6, progress)

	if err := fcm.SwitchToContext("details"); err != nil {
		return err
	}
	if filename != "" {
		maxWidth := 256 - 32
		if fcm.TTFDisplay.GetTextWidth(filename) > maxWidth {
			avgCharWidth := fcm.TTFDisplay.GetTextWidth("M")
			maxChars := maxWidth/avgCharWidth - 3
			if maxChars > 0 && maxChars < len(filename) {
				filename = filename[:maxChars] + "..."
			}
		}
		fcm.TTFDisplay.DrawTextCentered(filename, 46)
	}

	return nil
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
	fcm.TTFDisplay.DrawTextCentered(title, 24)

	// Progress bar (32 characters wide, centered)
	barWidth := 32
	barX := (256 - barWidth*8) / 2
	barY := 32
	fcm.TTFDisplay.DrawProgressBar(barX, barY, barWidth*8, 8, progress/100.0)

	// Percentage text
	if err := fcm.SwitchToContext("details"); err != nil {
		return err
	}
	percentText := fmt.Sprintf("%.0f%%", progress)
	fcm.TTFDisplay.DrawTextCentered(percentText, 46)

	// Details
	if details != "" {
		fcm.TTFDisplay.DrawTextCentered(details, 58)
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
		fcm.TTFDisplay.DrawTextCentered(title, titleY)
	}

	// Messages with regular font
	if err := fcm.SwitchToContext("menu"); err != nil {
		return err
	}

	if message1 != "" {
		fcm.TTFDisplay.DrawTextCentered(message1, message1Y)
	}

	if message2 != "" {
		fcm.TTFDisplay.DrawTextCentered(message2, message2Y)
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
		fcm.TTFDisplay.DrawText(96, yesNoY, yesText)

		if err := fcm.SwitchToContext("menu"); err != nil {
			return err
		}
		fcm.TTFDisplay.DrawText(160, yesNoY, noText)
	} else { // NO selected (default)
		if err := fcm.SwitchToContext("menu"); err != nil {
			return err
		}
		fcm.TTFDisplay.DrawText(96, yesNoY, yesText)

		if err := fcm.SwitchToContext("selected"); err != nil {
			return err
		}
		noText = "> NO"
		fcm.TTFDisplay.DrawText(160, yesNoY, noText)
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
		if fcm.TTFDisplay != nil && face == fcm.TTFDisplay.font {
			continue
		}
		face.Close()
	}
	if fcm.TTFDisplay != nil {
		return fcm.TTFDisplay.Close()
	}
	return nil
}
