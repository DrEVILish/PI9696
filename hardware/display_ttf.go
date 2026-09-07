package hardware

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/ioutil"
	"os"
	"time"

	"log/slog"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"
)

// simMode reports whether the app should run against a simulated display and
// GPIO instead of real hardware. Set PI9696_SIM=1 on dev machines (e.g. x86)
// that don't have SPI/GPIO, so the app can still start up and be exercised.
func simMode() bool {
	return os.Getenv("PI9696_SIM") != ""
}

// simFramePath returns where the simulator dumps the current framebuffer as
// a PNG on every Update(), so the rendered UI can be inspected without real
// hardware. Overridable via PI9696_SIM_OUT.
func simFramePath() string {
	if p := os.Getenv("PI9696_SIM_OUT"); p != "" {
		return p
	}
	return "/tmp/pi9696_sim_frame.png"
}

// renderScale is how much larger than the physical 256x64 panel the internal
// canvas is drawn at. Text is rasterized and positioned at this resolution,
// then box-filter downsampled to the panel's actual 4bpp/16-gray-level
// buffer - the antialiasing that survives that downsample is what makes
// glyph edges look smooth instead of jagged, even though the panel itself
// can't show more than 256x64 distinct dots. See canvasToBufferRect and
// EncodePNG.
const renderScale = 8

type TTFDisplay struct {
	sim     bool
	spiPort spi.PortCloser
	spiConn spi.Conn
	dcPin   gpio.PinOut
	resPin  gpio.PinOut
	buffer  []byte
	font    font.Face
	canvas  *image.Gray // renderScale times the panel's logical 256x64 resolution

	// brightnessPct is the last SetBrightness target (0-100). It tracks only
	// what was asked for, so auto-dim can restore the user's level.
	brightnessPct int
}

func NewTTFDisplay(fontPath string, fontSize float64) (*TTFDisplay, error) {
	sim := simMode()

	var spiPort spi.PortCloser
	var spiConn spi.Conn
	var dcPin, resPin gpio.PinOut

	if !sim {
		if _, err := host.Init(); err != nil {
			return nil, fmt.Errorf("failed to initialize periph: %v", err)
		}

		// Initialize SPI
		var err error
		spiPort, err = spireg.Open("")
		if err != nil {
			return nil, fmt.Errorf("failed to open SPI: %v", err)
		}

		spiConn, err = spiPort.Connect(10000000, spi.Mode0, 8)
		if err != nil {
			spiPort.Close()
			return nil, fmt.Errorf("failed to connect SPI: %v", err)
		}

		// Initialize GPIO pins
		dcPin = gpioreg.ByName("GPIO25")
		if dcPin == nil {
			spiPort.Close()
			return nil, fmt.Errorf("failed to get DC pin")
		}
		if err := dcPin.Out(gpio.Low); err != nil {
			spiPort.Close()
			return nil, fmt.Errorf("failed to set DC pin: %v", err)
		}

		resPin = gpioreg.ByName("GPIO24")
		if resPin == nil {
			spiPort.Close()
			return nil, fmt.Errorf("failed to get RES pin")
		}
		if err := resPin.Out(gpio.High); err != nil {
			spiPort.Close()
			return nil, fmt.Errorf("failed to set RES pin: %v", err)
		}
	}

	// Load TTF font
	fontFace, err := loadTTFFont(fontPath, fontSize)
	if err != nil {
		if spiPort != nil {
			spiPort.Close()
		}
		return nil, fmt.Errorf("failed to load font: %v", err)
	}

	d := &TTFDisplay{
		sim:           sim,
		spiPort:       spiPort,
		spiConn:       spiConn,
		dcPin:         dcPin,
		resPin:        resPin,
		buffer:        make([]byte, DisplayWidth*DisplayHeight/2), // 4 bits per pixel for SSD1322
		font:          fontFace,
		canvas:        image.NewGray(image.Rect(0, 0, DisplayWidth*renderScale, DisplayHeight*renderScale)),
		brightnessPct: 100,
	}

	if !sim {
		if err := d.init(); err != nil {
			d.Close()
			return nil, fmt.Errorf("failed to initialize display: %v", err)
		}
	}

	return d, nil
}

func loadTTFFont(fontPath string, fontSize float64) (font.Face, error) {
	// Read font file
	fontBytes, err := ioutil.ReadFile(fontPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read font file: %v", err)
	}

	// Parse TTF font
	ttfFont, err := opentype.Parse(fontBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse font: %v", err)
	}

	// Loaded at renderScale times the requested logical point size, to match
	// the supersampled canvas (see renderScale) - every caller of
	// loadTTFFont passes a logical size tuned for the 256x64 panel, and
	// this is the one place that scale gets applied so nothing downstream
	// (FiraCodeManager's per-context point sizes, TTFDisplay's coordinate
	// math) needs to know about it.
	fontFace, err := opentype.NewFace(ttfFont, &opentype.FaceOptions{
		Size:    fontSize * renderScale,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create font face: %v", err)
	}

	return fontFace, nil
}

func (d *TTFDisplay) init() error {
	// Reset display. SSD1322 requires RES# held low for at least 10us, then a
	// settle time before the init commands - the previous empty for-loop did
	// nothing measurable (Go drops it; even if it ran it's nanoseconds), which
	// left the panel unpredictable on real hardware.
	d.resPin.Out(gpio.Low)
	time.Sleep(20 * time.Millisecond)
	d.resPin.Out(gpio.High)
	time.Sleep(20 * time.Millisecond)

	// SSD1322 initialization sequence
	initSequence := [][]byte{
		{0xFD, 0x12},             // Unlock OLED driver IC
		{0xAE},                   // Display OFF
		{0xB3, 0x91},             // Display divide clockratio/oscillator frequency
		{0xCA, 0x3F},             // Multiplex ratio
		{0xA2, 0x00},             // Display offset
		{0xA1, 0x00},             // Display start line
		{0xA0, 0x14, 0x11},       // Set remap & dual COM line mode
		{0xB5, 0x00},             // GPIO
		{0xAB, 0x01},             // Function selection
		{0xB4, 0xA0, 0xB5, 0x55}, // Display enhancement
		{0xC1, 0x9F},             // Contrast current
		{0xC7, 0x0F},             // Master contrast current control
		{0xB1, 0xE2},             // Phase length
		{0xD1, 0x82, 0x20},       // Display enhancement B
		{0xBB, 0x1F},             // Precharge voltage
		{0xB6, 0x08},             // Second precharge period
		{0xBE, 0x07},             // VCOMH voltage
		{0xA6},                   // Normal display
		{0xAF},                   // Display ON
	}

	for _, cmd := range initSequence {
		if err := d.writeCommand(cmd); err != nil {
			return err
		}
	}

	return nil
}

// SetBrightness sets the panel's contrast current (SSD1322 command 0xC1,
// value 0x00-0xFF) from a 0-100 percentage - the "continuous brightness
// slider" of the Round 3 design. The init sequence also sets contrast; this
// is the runtime path used by the Settings/WebUI brightness control and by
// auto-dim, which drives the same command to a dim level then to 0 (panel
// effectively off) rather than using the display-sleep command, keeping a
// single brightness mechanism. No-op in sim/dev mode (no SPI bus).
func (d *TTFDisplay) SetBrightness(pct int) {
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	d.brightnessPct = pct
	if d.sim {
		return
	}
	d.writeCommand([]byte{0xC1, byte(pct * 255 / 100)})
}

func (d *TTFDisplay) writeCommand(cmd []byte) error {
	if d.sim {
		return nil
	}
	d.dcPin.Out(gpio.Low) // Command mode
	return d.spiConn.Tx(cmd, nil)
}

func (d *TTFDisplay) writeData(data []byte) error {
	if d.sim {
		return nil
	}
	d.dcPin.Out(gpio.High) // Data mode
	return d.spiConn.Tx(data, nil)
}

func (d *TTFDisplay) Clear() {
	// Clear canvas
	for i := range d.canvas.Pix {
		d.canvas.Pix[i] = 0
	}
	// Clear buffer
	for i := range d.buffer {
		d.buffer[i] = 0x00
	}
}

func (d *TTFDisplay) SetPixel(x, y int, brightness byte) {
	if x < 0 || x >= DisplayWidth || y < 0 || y >= DisplayHeight {
		return
	}

	// Fill the corresponding renderScale x renderScale block on the
	// supersampled canvas so shapes drawn via SetPixel (progress bars,
	// boxes) stay consistent with text if EncodePNG ever samples a region
	// that overlaps both - solid fills have no antialiasing to gain from
	// supersampling, but this keeps the canvas from having stale/blank data
	// under whatever SetPixel just drew.
	gray := color.Gray{Y: brightness * 17} // Scale 0-15 to 0-255
	draw.Draw(d.canvas, image.Rect(x*renderScale, y*renderScale, (x+1)*renderScale, (y+1)*renderScale), &image.Uniform{gray}, image.Point{}, draw.Src)

	// SSD1322 uses 4 bits per pixel, 2 pixels per byte
	bufferIndex := (y*DisplayWidth + x) / 2

	if x%2 == 0 {
		// Even pixel (upper nibble)
		d.buffer[bufferIndex] = (d.buffer[bufferIndex] & 0x0F) | ((brightness & 0x0F) << 4)
	} else {
		// Odd pixel (lower nibble)
		d.buffer[bufferIndex] = (d.buffer[bufferIndex] & 0xF0) | (brightness & 0x0F)
	}
}

func (d *TTFDisplay) DrawText(x, y int, text string) {
	// x, y and bounds are all logical (256x64) coordinates - getTextBounds
	// already divides the scaled font's measurements back down (see
	// renderScale), so this function is the only place that needs to know
	// about the internal supersampling; every caller keeps using the same
	// logical coordinate space as before.
	bounds := d.getTextBounds(text)

	// Clear the canvas area where text will be drawn, in scaled coordinates.
	// Padded a few logical px below the baseline for descenders (g, y, p, ...)
	// which extend past bounds.Max.Y - the same margin canvasToBufferRect
	// below uses so the panel buffer gets the same region refreshed.
	const descenderPad = 4
	logicalRect := image.Rect(x, y-bounds.Max.Y, x+bounds.Max.X, y+descenderPad)
	scaledRect := image.Rect(logicalRect.Min.X*renderScale, logicalRect.Min.Y*renderScale, logicalRect.Max.X*renderScale, logicalRect.Max.Y*renderScale)
	draw.Draw(d.canvas, scaledRect, &image.Uniform{color.Gray{0}}, image.Point{}, draw.Src)

	// Create a drawer for rendering text, at scaled coordinates using the
	// (already renderScale-sized) active face.
	drawer := &font.Drawer{
		Dst:  d.canvas,
		Src:  &image.Uniform{color.Gray{255}}, // White text
		Face: d.font,
		Dot:  fixed.Point26_6{X: fixed.I(x * renderScale), Y: fixed.I(y * renderScale)},
	}

	// Draw the text
	drawer.DrawString(text)

	// Downsample a slightly wider region than what was cleared/drawn -
	// getTextBounds discards BoundString's Min.X (a glyph with negative
	// left side bearing, e.g. italics, can ink left of x) and its
	// division-based scale-down can truncate the right edge by up to a
	// logical pixel, so drawn ink can land just outside logicalRect. That
	// ink still reaches the canvas (drawer.DrawString isn't clipped to
	// logicalRect), so if the downsample were clipped exactly to
	// logicalRect it could go stale in d.buffer while still visible in
	// EncodePNG's canvas-derived web mirror - the panel and the web mirror
	// would disagree. Padding only the downsample call (not the clear,
	// which would erase neighboring rows) covers that gap.
	//
	// render() calls DrawText many times per 100ms tick (once per visible menu row),
	// and a full 2048x512 box-filter downsample on every one of those would
	// multiply, not just add to, that cost.
	d.canvasToBufferRect(logicalRect.Inset(-2))
}

func (d *TTFDisplay) DrawTextCentered(text string, y int) {
	bounds := d.getTextBounds(text)
	x := (DisplayWidth - bounds.Max.X) / 2
	if x < 0 {
		x = 0
	}
	d.DrawText(x, y, text)
}

func (d *TTFDisplay) DrawTextRight(text string, y int, rightMargin int) {
	bounds := d.getTextBounds(text)
	x := DisplayWidth - bounds.Max.X - rightMargin
	if x < 0 {
		x = 0
	}
	d.DrawText(x, y, text)
}

// getTextBounds measures text using the active (renderScale-sized) face and
// scales the result back down to logical (256x64) units, so every caller -
// DrawText, DrawTextCentered, DrawTextRight, and everything in
// firacode_manager.go doing its own layout math against GetTextWidth - keeps
// working in the same coordinate space it always has, unaware the font
// backing it is internally 8x larger.
func (d *TTFDisplay) getTextBounds(text string) image.Rectangle {
	drawer := &font.Drawer{
		Face: d.font,
	}

	bounds, _ := drawer.BoundString(text)
	return image.Rectangle{
		Min: image.Point{X: 0, Y: 0},
		Max: image.Point{
			X: (int(bounds.Max.X-bounds.Min.X) >> 6) / renderScale, // Convert from fixed.Int26_6, then back to logical units
			Y: (int(bounds.Max.Y-bounds.Min.Y) >> 6) / renderScale,
		},
	}
}

func (d *TTFDisplay) GetTextWidth(text string) int {
	bounds := d.getTextBounds(text)
	return bounds.Max.X
}

func (d *TTFDisplay) GetFontHeight() int {
	metrics := d.font.Metrics()
	return (int(metrics.Height >> 6)) / renderScale // Convert from fixed.Int26_6, then back to logical units
}

// SetFontFace swaps the active font face used for subsequent text drawing.
// It does not touch the SPI/GPIO connection, canvas, or buffer, so callers
// can switch fonts mid-frame without losing anything already drawn.
func (d *TTFDisplay) SetFontFace(face font.Face) {
	d.font = face
}

// canvasToBufferRect box-filter downsamples the renderScale x renderScale
// block backing each logical pixel in rect (clipped to the panel's 256x64
// bounds) into the packed 4bpp SSD1322 buffer. Averaging rather than
// point-sampling is what turns the supersampled antialiasing into smoother
// gray levels on the physical panel instead of just picking one arbitrary
// subpixel per block.
func (d *TTFDisplay) canvasToBufferRect(rect image.Rectangle) {
	rect = rect.Intersect(image.Rect(0, 0, DisplayWidth, DisplayHeight))
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			var sum int
			for sy := 0; sy < renderScale; sy++ {
				for sx := 0; sx < renderScale; sx++ {
					sum += int(d.canvas.GrayAt(x*renderScale+sx, y*renderScale+sy).Y)
				}
			}
			avg := sum / (renderScale * renderScale)
			brightness := byte(avg / 17) // Convert 0-255 to 0-15
			if brightness > 15 {
				brightness = 15
			}

			bufferIndex := (y*DisplayWidth + x) / 2

			if x%2 == 0 {
				// Even pixel (upper nibble)
				d.buffer[bufferIndex] = (d.buffer[bufferIndex] & 0x0F) | ((brightness & 0x0F) << 4)
			} else {
				// Odd pixel (lower nibble)
				d.buffer[bufferIndex] = (d.buffer[bufferIndex] & 0xF0) | (brightness & 0x0F)
			}
		}
	}
}

func (d *TTFDisplay) DrawProgressBar(x, y, width, height int, progress float64) {
	// Draw progress bar background
	for py := y; py < y+height; py++ {
		for px := x; px < x+width; px++ {
			d.SetPixel(px, py, 2) // Dim background
		}
	}

	// Draw progress bar fill
	fillWidth := int(float64(width) * progress)
	for py := y; py < y+height; py++ {
		for px := x; px < x+fillWidth; px++ {
			d.SetPixel(px, py, 15) // Bright fill
		}
	}

	// Draw progress bar border
	for px := x; px < x+width; px++ {
		d.SetPixel(px, y, 8)          // Top border
		d.SetPixel(px, y+height-1, 8) // Bottom border
	}
	for py := y; py < y+height; py++ {
		d.SetPixel(x, py, 8)         // Left border
		d.SetPixel(x+width-1, py, 8) // Right border
	}
}

func (d *TTFDisplay) DrawBox(x, y, width, height int, brightness byte) {
	for py := y; py < y+height; py++ {
		for px := x; px < x+width; px++ {
			if px == x || px == x+width-1 || py == y || py == y+height-1 {
				d.SetPixel(px, py, brightness) // Border
			}
		}
	}
}

func (d *TTFDisplay) FillBox(x, y, width, height int, brightness byte) {
	for py := y; py < y+height; py++ {
		for px := x; px < x+width; px++ {
			d.SetPixel(px, py, brightness)
		}
	}
}

func (d *TTFDisplay) Update() error {
	if d.sim {
		return d.dumpSimFrame()
	}

	// Set column address
	if err := d.writeCommand([]byte{0x15, 0x1C, 0x5B}); err != nil {
		return err
	}
	// Set row address
	if err := d.writeCommand([]byte{0x75, 0x00, 0x3F}); err != nil {
		return err
	}
	// Write RAM command
	if err := d.writeCommand([]byte{0x5C}); err != nil {
		return err
	}
	// Send buffer data
	return d.writeData(d.buffer)
}

// dumpSimFrame writes the current canvas out as a PNG so the rendered UI can
// be inspected on a dev machine without a physical OLED attached.
func (d *TTFDisplay) dumpSimFrame() error {
	f, err := os.Create(simFramePath())
	if err != nil {
		return fmt.Errorf("failed to write sim frame: %v", err)
	}
	defer f.Close()
	// Decode from d.buffer, not d.canvas: real hardware only ever receives
	// the 4-bit packed buffer via Update(), so dumping the canvas directly
	// would only prove the text/shape drawing is correct and silently miss
	// a bug in the SetPixel/canvasToBufferRect nibble-packing path.
	return png.Encode(f, d.bufferToImage())
}

// bufferToImage decodes the SSD1322 4-bit-per-pixel, 2-pixels-per-byte
// buffer back into a grayscale image, mirroring what a physical panel would
// display, so sim screenshots are proof against the packing logic too.
func (d *TTFDisplay) bufferToImage() *image.Gray {
	img := image.NewGray(image.Rect(0, 0, DisplayWidth, DisplayHeight))
	for y := 0; y < DisplayHeight; y++ {
		for x := 0; x < DisplayWidth; x++ {
			bufferIndex := (y*DisplayWidth + x) / 2
			var nibble byte
			if x%2 == 0 {
				nibble = (d.buffer[bufferIndex] & 0xF0) >> 4
			} else {
				nibble = d.buffer[bufferIndex] & 0x0F
			}
			img.SetGray(x, y, color.Gray{Y: nibble * 17})
		}
	}
	return img
}

// webRenderScale is the resolution multiplier (relative to the panel's
// logical 256x64) served by EncodePNG to the WebUI mirror. It's lower than
// renderScale (8x): the panel itself is 4bpp/16 gray levels, so the full 8x
// canvas is buying antialiasing quality on the way down to that, not detail
// worth shipping whole over the network - at 8x a frame is ~2048x512, and
// the remote dashboard polls this endpoint several times a second over the
// same eth0 link Inferno's audio is on. 4x keeps genuinely sharp edges
// (still well above what glyph antialiasing needs) at a quarter the pixels.
const webRenderScale = 4

// EncodePNG writes the current frame as a PNG for the WebUI mirror,
// downsampled from the supersampled canvas (see renderScale) rather than
// the packed 4bpp panel buffer - that's the whole point of rendering at 8x
// internally: the panel is stuck at 256x64/16 gray levels, but the web
// mirror doesn't have to be. Callers needing a consistent snapshot (not
// torn by a concurrent render()) must hold the app mutex.
func (d *TTFDisplay) EncodePNG(w io.Writer) error {
	return png.Encode(w, downsampleGray(d.canvas, renderScale/webRenderScale))
}

// downsampleGray box-filters src down by factor in both dimensions,
// averaging each factor x factor block into one output pixel.
func downsampleGray(src *image.Gray, factor int) *image.Gray {
	b := src.Bounds()
	outW, outH := b.Dx()/factor, b.Dy()/factor
	out := image.NewGray(image.Rect(0, 0, outW, outH))
	for y := 0; y < outH; y++ {
		for x := 0; x < outW; x++ {
			var sum int
			for sy := 0; sy < factor; sy++ {
				for sx := 0; sx < factor; sx++ {
					sum += int(src.GrayAt(b.Min.X+x*factor+sx, b.Min.Y+y*factor+sy).Y)
				}
			}
			out.SetGray(x, y, color.Gray{Y: byte(sum / (factor * factor))})
		}
	}
	return out
}

// DrawStatusBarWithIcons draws the status bar as compact bracketed text
// indicators (e.g. "4GB [USB]", "[ETH]", "[INF]"), right-aligned using the
// same TTF font as the rest of the UI.
//
// Earlier versions mixed three incompatible approaches here: the full TTF
// font, a hand-rolled 5x7 bitmap font that only defined glyphs for "U", "S",
// "B" and "-" (so "ETH" silently drew nothing), and 16x16 icon bitmaps that
// don't fit inside a 12px-tall status bar. Plain text sidesteps all of that
// and matches the original design's bracket notation.
func (d *TTFDisplay) DrawStatusBarWithIcons(formatInfo, usbInfo string, usbConnected bool, networkConnected bool, networkInfo string, infernoRunning bool) {
	// Clear status bar area
	d.FillBox(0, 0, DisplayWidth, 12, 0)

	// Format info on the left
	d.DrawText(2, 10, formatInfo)

	netLabel := "[---]"
	if networkConnected {
		netLabel = "[ETH]"
	}
	infLabel := ""
	if infernoRunning {
		infLabel = "[INF]"
	}

	x := DisplayWidth - 4
	for _, label := range []string{usbInfo, netLabel, infLabel} {
		if label == "" {
			continue
		}
		x -= d.GetTextWidth(label)
		d.DrawText(x, 10, label)
		x -= 8 // gap before the next indicator
	}
}

func (d *TTFDisplay) Close() error {
	if d.font != nil {
		if err := d.font.Close(); err != nil {
			slog.Warn(fmt.Sprintf("failed to close font: %v", err))
		}
	}
	if d.spiConn != nil {
		d.spiConn = nil
	}
	if d.spiPort != nil {
		return d.spiPort.Close()
	}
	return nil
}

// Helper function to create display with default font if TTF loading fails
func NewDisplayWithFallback(fontPath string, fontSize float64) (*TTFDisplay, error) {
	// Try to load TTF font first
	display, err := NewTTFDisplay(fontPath, fontSize)
	if err != nil {
		slog.Warn(fmt.Sprintf("failed to load TTF font, falling back to bitmap font: %v", err))
		// Could fallback to original bitmap font implementation here
		return nil, err
	}
	return display, nil
}
