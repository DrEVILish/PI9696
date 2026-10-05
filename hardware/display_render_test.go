package hardware

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/math/fixed"
)

// newTestDisplay uses the panel's FiraCode when the system font package is
// installed (fonts/ symlinks into it), else the Go font shipped with
// x/image, so the tests run on machines without it too.
func newTestDisplay(t testing.TB, size float64) *TTFDisplay {
	t.Helper()
	path := "../fonts/FiraCode-Regular.ttf"
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(t.TempDir(), "goregular.ttf")
		if err := os.WriteFile(path, goregular.TTF, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	face, err := loadTTFFont(path, size)
	if err != nil {
		t.Fatalf("load font: %v", err)
	}
	return &TTFDisplay{
		buffer: make([]byte, DisplayWidth*DisplayHeight/2),
		canvas: image.NewGray(image.Rect(0, 0, DisplayWidth*renderScale, DisplayHeight*renderScale)),
		font:   face,
	}
}

// The table-driven glyph blit must leave exactly the pixels font.Drawer's
// generic draw.DrawMask path leaves, including over existing gray content
// and at the canvas edges.
func TestDrawStringMatchesFontDrawer(t *testing.T) {
	d := newTestDisplay(t, 11)
	rng := rand.New(rand.NewSource(1))
	texts := []string{"REC 00:12:34", "48kHz 24bit 64ch", "gjpqy|{}[]→≥≠", "Ωµ° ±dBFS", "", "WWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWW"}
	for i, text := range texts {
		for trial := 0; trial < 4; trial++ {
			rng.Read(d.canvas.Pix)
			ref := image.NewGray(d.canvas.Rect)
			copy(ref.Pix, d.canvas.Pix)
			before := append([]byte(nil), d.canvas.Pix...)
			// Positions include ones that clip off every edge.
			dot := fixed.Point26_6{X: fixed.I(rng.Intn(2300) - 150), Y: fixed.I(rng.Intn(700) - 50)}
			(&font.Drawer{Dst: ref, Src: image.NewUniform(color.Gray{255}), Face: d.font, Dot: dot}).DrawString(text)
			d.drawString(dot, text)
			if !bytes.Equal(ref.Pix, d.canvas.Pix) {
				t.Fatalf("text %d trial %d (%q at %v): canvas differs from font.Drawer", i, trial, text, dot)
			}
			// Again from the glyph cache, which the first pass filled.
			copy(d.canvas.Pix, before)
			d.drawString(dot, text)
			if !bytes.Equal(ref.Pix, d.canvas.Pix) {
				t.Fatalf("text %d trial %d (%q at %v): cached glyphs differ from font.Drawer", i, trial, text, dot)
			}
		}
	}
}

func TestOverWhiteFastCasesMatchTable(t *testing.T) {
	overWhiteOnce.Do(initOverWhite)
	for v := 0; v < 256; v++ {
		if got := overWhite[v<<8]; got != byte(v) {
			t.Fatalf("mask 0 over %d = %d, want unchanged", v, got)
		}
		if got := overWhite[v<<8|255]; got != 255 {
			t.Fatalf("mask 255 over %d = %d, want 255", v, got)
		}
	}
}

// SetPixel and FillBox write Pix directly now; they must match the old
// draw.Draw-of-a-Uniform result and set the same panel nibbles.
func TestFillPathsMatchReference(t *testing.T) {
	d := newTestDisplay(t, 11)
	ref := newTestDisplay(t, 11)
	refSet := func(x, y int, b byte) {
		if x < 0 || x >= DisplayWidth || y < 0 || y >= DisplayHeight {
			return
		}
		draw.Draw(ref.canvas, image.Rect(x*renderScale, y*renderScale, (x+1)*renderScale, (y+1)*renderScale), image.NewUniform(color.Gray{b * 17}), image.Point{}, draw.Src)
		ref.setNibble(x, y, b)
	}
	d.DrawProgressBar(10, 20, 100, 6, 0.37)
	d.FillBox(250, 60, 20, 20, 9) // clipped at the corner
	d.FillBox(-5, -5, 12, 3, 4)   // clipped at the origin
	d.DrawBox(30, 30, 40, 10, 12)
	d.SetPixel(-1, 3, 15)
	d.DrawProgressBar(200, 2, 0, 3, 0.5) // zero width still draws both side borders
	d.DrawBox(5, 50, 7, 0, 3)            // zero height draws nothing
	d.DrawBox(5, 52, 7, 1, 6)            // one row
	for py := 20; py < 26; py++ {
		for px := 10; px < 110; px++ {
			refSet(px, py, 2)
		}
	}
	for py := 20; py < 26; py++ {
		for px := 10; px < 10+int(float64(100)*0.37); px++ {
			refSet(px, py, 15)
		}
	}
	for px := 10; px < 110; px++ {
		refSet(px, 20, 8)
		refSet(px, 25, 8)
	}
	for py := 20; py < 26; py++ {
		refSet(10, py, 8)
		refSet(109, py, 8)
	}
	for py := 60; py < 80; py++ {
		for px := 250; px < 270; px++ {
			refSet(px, py, 9)
		}
	}
	for py := -5; py < -2; py++ {
		for px := -5; px < 7; px++ {
			refSet(px, py, 4)
		}
	}
	for py := 30; py < 40; py++ {
		for px := 30; px < 70; px++ {
			if px == 30 || px == 69 || py == 30 || py == 39 {
				refSet(px, py, 12)
			}
		}
	}
	for py := 2; py < 5; py++ {
		refSet(200, py, 8)
		refSet(199, py, 8)
	}
	for px := 5; px < 12; px++ {
		refSet(px, 52, 6)
	}
	if !bytes.Equal(ref.canvas.Pix, d.canvas.Pix) {
		t.Error("canvas differs from draw.Draw reference")
	}
	if !bytes.Equal(ref.buffer, d.buffer) {
		t.Error("panel buffer differs from reference")
	}
}

// CanvasHash replaced a 1MB checksum with a hash of the draw calls: the
// same frame redrawn must keep it, and any visible change must move it.
func TestCanvasHashTracksDrawCalls(t *testing.T) {
	d := newTestDisplay(t, 11)
	frame := func(text string, fill byte) uint64 {
		d.Clear()
		d.DrawText(4, 20, text)
		d.FillBox(0, 40, 50, 4, fill)
		return d.CanvasHash()
	}
	a := frame("MENU", 5)
	if b := frame("MENU", 5); b != a {
		t.Fatal("identical frame changed CanvasHash")
	}
	if b := frame("MENV", 5); b == a {
		t.Error("different text kept CanvasHash")
	}
	if b := frame("MENU", 6); b == a {
		t.Error("different fill kept CanvasHash")
	}
	other := newTestDisplay(t, 14)
	d.Clear()
	d.SetFontFace(other.font)
	d.DrawText(4, 20, "MENU")
	d.FillBox(0, 40, 50, 4, 5)
	if d.CanvasHash() == a {
		t.Error("different face kept CanvasHash")
	}
}

func BenchmarkRenderMenuFrame(b *testing.B) {
	d := newTestDisplay(b, 11)
	for i := 0; i < b.N; i++ {
		d.Clear()
		for row := 0; row < 5; row++ {
			d.DrawText(2, 12+row*12, "Settings > Logging: info")
		}
		d.DrawProgressBar(4, 58, 240, 5, 0.5)
		_ = d.FrameHash()
		_ = d.CanvasHash()
	}
}

// canvasToBufferRect's 64-bit block sums must match a plain per-pixel box
// average.
func TestCanvasToBufferMatchesBoxAverage(t *testing.T) {
	d := newTestDisplay(t, 11)
	rand.New(rand.NewSource(2)).Read(d.canvas.Pix)
	for i := 0; i < 2048; i++ { // saturated blocks hit the 2040 lane max
		d.canvas.Pix[i] = 255
	}
	d.canvasToBufferRect(image.Rect(-3, -3, DisplayWidth+3, DisplayHeight+3))
	ref := newTestDisplay(t, 11)
	for y := 0; y < DisplayHeight; y++ {
		for x := 0; x < DisplayWidth; x++ {
			var sum int
			for sy := 0; sy < renderScale; sy++ {
				for sx := 0; sx < renderScale; sx++ {
					sum += int(d.canvas.GrayAt(x*renderScale+sx, y*renderScale+sy).Y)
				}
			}
			ref.setNibble(x, y, byte(min(sum/(renderScale*renderScale)/17, 15)))
		}
	}
	if !bytes.Equal(ref.buffer, d.buffer) {
		t.Error("downsampled buffer differs from per-pixel box average")
	}
}
