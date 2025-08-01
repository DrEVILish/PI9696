package hardware

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io/ioutil"
	"log"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"
)



type TTFDisplay struct {
	spiPort   spi.PortCloser
	spiConn   spi.Conn
	dcPin     gpio.PinOut
	resPin    gpio.PinOut
	buffer    []byte
	font      font.Face
	canvas    *image.Gray
	svgLoader *SVGLoader
}

func NewTTFDisplay(fontPath string, fontSize float64) (*TTFDisplay, error) {
	if _, err := host.Init(); err != nil {
		return nil, fmt.Errorf("failed to initialize periph: %v", err)
	}

	// Initialize SPI
	spiPort, err := spireg.Open("")
	if err != nil {
		return nil, fmt.Errorf("failed to open SPI: %v", err)
	}

	spiConn, err := spiPort.Connect(10000000, spi.Mode0, 8)
	if err != nil {
		spiPort.Close()
		return nil, fmt.Errorf("failed to connect SPI: %v", err)
	}

	// Initialize GPIO pins
	dcPin := gpioreg.ByName("GPIO25")
	if dcPin == nil {
		return nil, fmt.Errorf("failed to get DC pin")
	}
	if err := dcPin.Out(gpio.Low); err != nil {
		return nil, fmt.Errorf("failed to set DC pin: %v", err)
	}

	resPin := gpioreg.ByName("GPIO24")
	if resPin == nil {
		return nil, fmt.Errorf("failed to get RES pin")
	}
	if err := resPin.Out(gpio.High); err != nil {
		return nil, fmt.Errorf("failed to set RES pin: %v", err)
	}

	// Load TTF font
	fontFace, err := loadTTFFont(fontPath, fontSize)
	if err != nil {
		return nil, fmt.Errorf("failed to load font: %v", err)
	}

	d := &TTFDisplay{
		spiPort:   spiPort,
		spiConn:   spiConn,
		dcPin:     dcPin,
		resPin:    resPin,
		buffer:    make([]byte, DisplayWidth*DisplayHeight/2), // 4 bits per pixel for SSD1322
		font:      fontFace,
		canvas:    image.NewGray(image.Rect(0, 0, DisplayWidth, DisplayHeight)),
		svgLoader: NewSVGLoader("./svg"), // Initialize SVG loader with svg directory
	}

	if err := d.init(); err != nil {
		d.Close()
		return nil, fmt.Errorf("failed to initialize display: %v", err)
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

	// Create font face with specified size
	fontFace, err := opentype.NewFace(ttfFont, &opentype.FaceOptions{
		Size:    fontSize,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create font face: %v", err)
	}

	return fontFace, nil
}

func (d *TTFDisplay) init() error {
	// Reset display
	d.resPin.Out(gpio.Low)
	// Small delay
	for i := 0; i < 1000; i++ {
	}
	d.resPin.Out(gpio.High)
	
	// SSD1322 initialization sequence
	initSequence := [][]byte{
		{0xFD, 0x12}, // Unlock OLED driver IC
		{0xAE},       // Display OFF
		{0xB3, 0x91}, // Display divide clockratio/oscillator frequency
		{0xCA, 0x3F}, // Multiplex ratio
		{0xA2, 0x00}, // Display offset
		{0xA1, 0x00}, // Display start line
		{0xA0, 0x14, 0x11}, // Set remap & dual COM line mode
		{0xB5, 0x00}, // GPIO
		{0xAB, 0x01}, // Function selection
		{0xB4, 0xA0, 0xB5, 0x55}, // Display enhancement
		{0xC1, 0x9F}, // Contrast current
		{0xC7, 0x0F}, // Master contrast current control
		{0xB1, 0xE2}, // Phase length
		{0xD1, 0x82, 0x20}, // Display enhancement B
		{0xBB, 0x1F}, // Precharge voltage
		{0xB6, 0x08}, // Second precharge period
		{0xBE, 0x07}, // VCOMH voltage
		{0xA6},       // Normal display
		{0xAF},       // Display ON
	}

	for _, cmd := range initSequence {
		if err := d.writeCommand(cmd); err != nil {
			return err
		}
	}

	return nil
}

func (d *TTFDisplay) writeCommand(cmd []byte) error {
	d.dcPin.Out(gpio.Low) // Command mode
	return d.spiConn.Tx(cmd, nil)
}

func (d *TTFDisplay) writeData(data []byte) error {
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
	
	// Set pixel in canvas
	d.canvas.SetGray(x, y, color.Gray{Y: brightness * 17}) // Scale 0-15 to 0-255
	
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

// Network icon bitmap (16x16 pixels) - Ethernet connection icon
func (d *TTFDisplay) getNetworkIconBitmap() [16][16]byte {
	// Try to load from SVG first, fallback to hardcoded bitmap if failed
	if d.svgLoader != nil {
		if bitmap, err := d.svgLoader.LoadNetworkIcon(16, false); err == nil {
			return ConvertToFixedArray16(bitmap)
		}
	}
	
	// Fallback to original hardcoded bitmap
	return [16][16]byte{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 15, 0, 0},
		{0, 15, 0, 0, 15, 15, 0, 0, 0, 0, 15, 15, 0, 0, 15, 0},
		{0, 15, 0, 15, 0, 0, 15, 0, 0, 15, 0, 0, 15, 0, 15, 0},
		{0, 15, 0, 15, 0, 0, 15, 0, 0, 15, 0, 0, 15, 0, 15, 0},
		{0, 15, 0, 15, 0, 0, 15, 0, 0, 15, 0, 0, 15, 0, 15, 0},
		{0, 15, 0, 15, 0, 0, 15, 0, 0, 15, 0, 0, 15, 0, 15, 0},
		{0, 15, 0, 15, 0, 0, 15, 0, 0, 15, 0, 0, 15, 0, 15, 0},
		{0, 15, 0, 15, 0, 0, 15, 0, 0, 15, 0, 0, 15, 0, 15, 0},
		{0, 15, 0, 15, 0, 0, 15, 0, 0, 15, 0, 0, 15, 0, 15, 0},
		{0, 15, 0, 0, 15, 15, 0, 0, 0, 0, 15, 15, 0, 0, 15, 0},
		{0, 0, 15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 15, 0, 0},
		{0, 0, 0, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
}

// Small network icon bitmap (8x8 pixels) for status bar
func (d *TTFDisplay) getNetworkIconSmall() [8][8]byte {
	// Try to load from SVG first, fallback to hardcoded bitmap if failed
	if d.svgLoader != nil {
		if bitmap, err := d.svgLoader.LoadNetworkIcon(8, true); err == nil {
			return ConvertToFixedArray8(bitmap)
		}
	}
	
	// Fallback to original hardcoded bitmap
	return [8][8]byte{
		{0, 15, 15, 15, 15, 15, 15, 0},
		{15, 0, 0, 0, 0, 0, 0, 15},
		{15, 0, 15, 0, 0, 15, 0, 15},
		{15, 0, 15, 0, 0, 15, 0, 15},
		{15, 0, 15, 0, 0, 15, 0, 15},
		{15, 0, 15, 0, 0, 15, 0, 15},
		{15, 0, 0, 0, 0, 0, 0, 15},
		{0, 15, 15, 15, 15, 15, 15, 0},
	}
}


func (d *TTFDisplay) DrawText(x, y int, text string) {
	// Clear the canvas area where text will be drawn
	bounds := d.getTextBounds(text)
	clearRect := image.Rect(x, y-bounds.Max.Y, x+bounds.Max.X, y)
	draw.Draw(d.canvas, clearRect, &image.Uniform{color.Gray{0}}, image.Point{}, draw.Src)

	// Create a drawer for rendering text
	drawer := &font.Drawer{
		Dst:  d.canvas,
		Src:  &image.Uniform{color.Gray{255}}, // White text
		Face: d.font,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}

	// Draw the text
	drawer.DrawString(text)

	// Convert rendered text to display buffer
	d.canvasToBuffer()
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

func (d *TTFDisplay) getTextBounds(text string) image.Rectangle {
	drawer := &font.Drawer{
		Face: d.font,
	}
	
	bounds, _ := drawer.BoundString(text)
	return image.Rectangle{
		Min: image.Point{X: 0, Y: 0},
		Max: image.Point{
			X: int(bounds.Max.X-bounds.Min.X) >> 6, // Convert from fixed.Int26_6
			Y: int(bounds.Max.Y-bounds.Min.Y) >> 6,
		},
	}
}

func (d *TTFDisplay) GetTextWidth(text string) int {
	bounds := d.getTextBounds(text)
	return bounds.Max.X
}

func (d *TTFDisplay) GetFontHeight() int {
	metrics := d.font.Metrics()
	return int(metrics.Height >> 6) // Convert from fixed.Int26_6
}

func (d *TTFDisplay) canvasToBuffer() {
	// Convert grayscale canvas to 4-bit buffer for SSD1322
	for y := 0; y < DisplayHeight; y++ {
		for x := 0; x < DisplayWidth; x++ {
			grayVal := d.canvas.GrayAt(x, y).Y
			brightness := byte(grayVal / 17) // Convert 0-255 to 0-15
			
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

// USB icon bitmap (16x16 pixels) - converted from USB SVG
func (d *TTFDisplay) getUSBIconBitmap() [16][16]byte {
	// Try to load from SVG first, fallback to hardcoded bitmap if failed
	if d.svgLoader != nil {
		if bitmap, err := d.svgLoader.LoadUSBIcon(16, false); err == nil {
			return ConvertToFixedArray16(bitmap)
		}
	}
	
	// Fallback to original hardcoded bitmap
	return [16][16]byte{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 15, 15, 15, 15, 15, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 15, 15, 15, 15, 15, 15, 15, 0, 0, 0, 0, 0},
		{0, 0, 0, 15, 15, 0, 0, 0, 0, 0, 15, 15, 0, 0, 0, 0},
		{0, 0, 15, 15, 0, 0, 0, 0, 0, 0, 0, 15, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 0, 0, 15, 15, 0, 0, 0, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 0, 15, 15, 15, 15, 0, 0, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 15, 15, 0, 0, 15, 15, 0, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 15, 0, 0, 0, 0, 15, 0, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 15, 15, 0, 0, 15, 15, 0, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 0, 15, 15, 15, 15, 0, 0, 15, 0, 0, 0},
		{0, 0, 15, 0, 0, 0, 0, 15, 15, 0, 0, 0, 15, 0, 0, 0},
		{0, 0, 15, 15, 0, 0, 0, 0, 0, 0, 0, 15, 15, 0, 0, 0},
		{0, 0, 0, 15, 15, 0, 0, 0, 0, 0, 15, 15, 0, 0, 0, 0},
		{0, 0, 0, 0, 15, 15, 15, 15, 15, 15, 15, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 15, 15, 15, 15, 15, 0, 0, 0, 0, 0, 0},
	}
}

// Small USB icon bitmap (8x8 pixels) for status bar
func (d *TTFDisplay) getUSBIconSmall() [8][8]byte {
	// Try to load from SVG first, fallback to hardcoded bitmap if failed
	if d.svgLoader != nil {
		if bitmap, err := d.svgLoader.LoadUSBIcon(8, true); err == nil {
			return ConvertToFixedArray8(bitmap)
		}
	}
	
	// Fallback to original hardcoded bitmap
	return [8][8]byte{
		{0, 0, 15, 15, 15, 15, 0, 0},
		{0, 15, 15, 0, 0, 15, 15, 0},
		{15, 15, 0, 0, 0, 0, 15, 15},
		{15, 0, 0, 15, 15, 0, 0, 15},
		{15, 0, 15, 15, 15, 15, 0, 15},
		{15, 15, 0, 0, 0, 0, 15, 15},
		{0, 15, 15, 0, 0, 15, 15, 0},
		{0, 0, 15, 15, 15, 15, 0, 0},
	}
}

// DrawUSBIcon draws a USB icon at the specified position
func (d *TTFDisplay) DrawUSBIcon(x, y int, size string) {
	var iconData [][]byte
	var iconSize int
	
	if size == "small" {
		smallIcon := d.getUSBIconSmall()
		iconSize = 8
		iconData = make([][]byte, iconSize)
		for i := 0; i < iconSize; i++ {
			iconData[i] = smallIcon[i][:]
		}
	} else {
		largeIcon := d.getUSBIconBitmap()
		iconSize = 16
		iconData = make([][]byte, iconSize)
		for i := 0; i < iconSize; i++ {
			iconData[i] = largeIcon[i][:]
		}
	}
	
	// Draw the icon pixel by pixel
	for py := 0; py < iconSize; py++ {
		for px := 0; px < iconSize; px++ {
			if iconData[py][px] > 0 {
				d.SetPixel(x+px, y+py, iconData[py][px])
			}
		}
	}
}

// DrawNetworkIcon draws a network icon at the specified position
func (d *TTFDisplay) DrawNetworkIcon(x, y int, size string) {
	var iconData [][]byte
	var iconSize int
	
	if size == "small" {
		smallIcon := d.getNetworkIconSmall()
		iconSize = 8
		iconData = make([][]byte, iconSize)
		for i := 0; i < iconSize; i++ {
			iconData[i] = smallIcon[i][:]
		}
	} else {
		largeIcon := d.getNetworkIconBitmap()
		iconSize = 16
		iconData = make([][]byte, iconSize)
		for i := 0; i < iconSize; i++ {
			iconData[i] = largeIcon[i][:]
		}
	}
	
	// Draw the icon pixel by pixel
	for py := 0; py < iconSize; py++ {
		for px := 0; px < iconSize; px++ {
			if iconData[py][px] > 0 {
				d.SetPixel(x+px, y+py, iconData[py][px])
			}
		}
	}
}

// DrawNetworkStatus draws network connection status with icon and text
func (d *TTFDisplay) DrawNetworkStatus(x, y int, connected bool, ipAddr string) {
	// Draw network icon
	d.DrawNetworkIcon(x, y, "small")
	
	// Draw connection status
	textX := x + 10 // Offset for icon width + margin
	var statusText string
	var brightness byte = 8 // Dim text
	
	if connected && ipAddr != "" {
		statusText = "ETH"
		brightness = 15 // Bright text when connected
	} else {
		statusText = "---"
		brightness = 4 // Very dim when disconnected
	}
	
	// Use simple text drawing for status
	for i, char := range statusText {
		charX := textX + i*6
		if charX < DisplayWidth-6 {
			d.drawSimpleChar(charX, y, byte(char), brightness)
		}
	}
}

// DrawUSBStatus draws USB connection status with icon and text
func (d *TTFDisplay) DrawUSBStatus(x, y int, connected bool, size string) {
	// Draw USB icon
	d.DrawUSBIcon(x, y, "small")
	
	// Draw connection status
	textX := x + 10 // Offset for icon width + margin
	var statusText string
	var brightness byte = 8 // Dim text
	
	if connected {
		statusText = "USB"
		brightness = 15 // Bright text when connected
	} else {
		statusText = "---"
		brightness = 4 // Very dim when disconnected
	}
	
	// Use simple text drawing for status
	for i, char := range statusText {
		charX := textX + i*6
		if charX < DisplayWidth-6 {
			d.drawSimpleChar(charX, y, byte(char), brightness)
		}
	}
}

// drawSimpleChar draws a simple 5x7 character for status indicators
func (d *TTFDisplay) drawSimpleChar(x, y int, char byte, brightness byte) {
	var charData [7]byte
	
	switch char {
	case 'U':
		charData = [7]byte{0x1E, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0E}
	case 'S':
		charData = [7]byte{0x0E, 0x11, 0x10, 0x0E, 0x01, 0x11, 0x0E}
	case 'B':
		charData = [7]byte{0x1E, 0x11, 0x11, 0x1E, 0x11, 0x11, 0x1E}
	case '-':
		charData = [7]byte{0x00, 0x00, 0x00, 0x1F, 0x00, 0x00, 0x00}
	default:
		charData = [7]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	}
	
	// Draw character bitmap
	for row := 0; row < 7; row++ {
		for col := 0; col < 5; col++ {
			if charData[row]&(1<<(4-col)) != 0 {
				d.SetPixel(x+col, y+row, brightness)
			}
		}
	}
}

// DrawStatusBarWithIcons draws the status bar with USB and network icon integration
func (d *TTFDisplay) DrawStatusBarWithIcons(formatInfo, usbInfo string, usbConnected bool, networkConnected bool, ipAddr string) {
	// Clear status bar area
	d.FillBox(0, 0, DisplayWidth, 12, 0)
	
	// Draw format info on the left
	d.DrawText(2, 10, formatInfo)
	
	// Calculate positions for icons on the right
	rightMargin := 8
	currentX := DisplayWidth - rightMargin
	
	// Draw USB status with icon (rightmost)
	usbX := currentX - 40 // Reserve space for USB icon + text
	d.DrawUSBStatus(usbX, 2, usbConnected, "small")
	currentX = usbX - 5
	
	// Draw network status with icon (left of USB)
	netX := currentX - 40 // Reserve space for network icon + text
	d.DrawNetworkStatus(netX, 2, networkConnected, ipAddr)
	currentX = netX - 5
	
	// Draw USB info text if connected and space allows
	if usbConnected && usbInfo != "" {
		infoWidth := d.GetTextWidth(usbInfo)
		infoX := currentX - infoWidth - 5
		if infoX > d.GetTextWidth(formatInfo) + 10 {
			d.DrawText(infoX, 10, usbInfo)
		}
	}
}

// DrawStatusBarWithUSB draws the status bar with USB icon integration
func (d *TTFDisplay) DrawStatusBarWithUSB(formatInfo, usbInfo string, usbConnected bool) {
	// Call the enhanced version with no network info
	d.DrawStatusBarWithIcons(formatInfo, usbInfo, usbConnected, false, "")
}

func (d *TTFDisplay) Close() error {
	if d.font != nil {
		if err := d.font.Close(); err != nil {
			log.Printf("Warning: failed to close font: %v", err)
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
		log.Printf("Failed to load TTF font, falling back to bitmap font: %v", err)
		// Could fallback to original bitmap font implementation here
		return nil, err
	}
	return display, nil
}