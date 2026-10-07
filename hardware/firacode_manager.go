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
	*TTFDisplay                   // its drawing primitives are used directly
	faces       map[string]string // weight ("Regular", "Bold", ...) -> font file
	currentFont string
	currentSize float64
	fontFaces   map[string]font.Face // cache keyed by "path@size", avoids re-parsing TTFs on every context switch
}

// contextFont is the weight and point size a UI context draws with.
type contextFont struct {
	weight string
	pt     float64
}

// contextFonts maps the OLED's font contexts to their faces. Any other
// context (e.g. "idle") uses defaultContextFont. Weights missing from the
// install fall back to Regular at the same size (see SwitchToContext).
var contextFonts = map[string]contextFont{
	"statusbar": {"Regular", 9},
	"menu":      {"Regular", 10},
	"selected":  {"SemiBold", 10},
	"header":    {"Medium", 13},
	"details":   {"Light", 8},
	"recording": {"Bold", 14},
	"alert":     {"Bold", 14},
}

var defaultContextFont = contextFont{"Regular", 11}

func fontForContext(context string) contextFont {
	if f, ok := contextFonts[context]; ok {
		return f
	}
	return defaultContextFont
}

// fallbackFaces is the system-font (DejaVu) set used when the FiraCode
// files cannot be loaded.
func fallbackFaces() map[string]string {
	return map[string]string{
		"Regular": "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"Bold":    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
	}
}

func NewFiraCodeManager() (*FiraCodeManager, error) {
	faces := map[string]string{}
	for _, w := range []string{"Regular", "Bold", "Light", "Medium", "SemiBold"} {
		faces[w] = filepath.Join("./fonts", "FiraCode-"+w+".ttf")
	}
	if err := validateFaces(faces); err != nil {
		return nil, fmt.Errorf("FiraCode validation failed: %v", err)
	}

	// Initialize with regular font at main content size
	display, err := NewTTFDisplay(faces["Regular"], defaultContextFont.pt)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize FiraCode display: %v", err)
	}

	manager := &FiraCodeManager{
		TTFDisplay:  display,
		faces:       faces,
		currentFont: faces["Regular"],
		currentSize: defaultContextFont.pt,
		fontFaces:   make(map[string]font.Face),
	}
	manager.fontFaces[fontFaceKey(faces["Regular"], defaultContextFont.pt)] = display.font

	slog.Info("FiraCode manager initialized successfully")
	return manager, nil
}

// fontFaceKey builds the cache key for a given font path and point size.
func fontFaceKey(fontPath string, fontSize float64) string {
	return fmt.Sprintf("%s@%.1f", fontPath, fontSize)
}

// validateFaces requires the Regular and Bold files and warns about any
// other missing weight.
func validateFaces(faces map[string]string) error {
	var missingRequired, missingOptional []string
	for w, path := range faces {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if w == "Regular" || w == "Bold" {
				missingRequired = append(missingRequired, fmt.Sprintf("%s (%s)", w, path))
			} else {
				missingOptional = append(missingOptional, w)
			}
		}
	}
	if len(missingRequired) > 0 {
		return fmt.Errorf("missing required FiraCode fonts: %v", missingRequired)
	}
	if len(missingOptional) > 0 {
		slog.Warn(fmt.Sprintf("Optional FiraCode fonts not found: %v", missingOptional))
	}
	slog.Info(fmt.Sprintf("FiraCode fonts validated: Regular=%s, Bold=%s", faces["Regular"], faces["Bold"]))
	return nil
}

// SwitchToContext changes font and size based on UI context. A missing
// optional face (Light/SemiBold/Medium installs vary) falls back to Regular
// at the requested size: without this a failed switch leaves the previous
// context's face stuck, since most draw callers ignore the error return.
func (fcm *FiraCodeManager) SwitchToContext(context string) error {
	f := fontForContext(context)
	fontPath, fontSize := fcm.faces[f.weight], f.pt

	if fontPath == fcm.currentFont && fontSize == fcm.currentSize {
		return nil // Already using correct font/size
	}

	if err := fcm.switchFont(fontPath, fontSize); err != nil {
		if fontPath == fcm.faces["Regular"] {
			return err
		}
		return fcm.switchFont(fcm.faces["Regular"], fontSize)
	}
	return nil
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
