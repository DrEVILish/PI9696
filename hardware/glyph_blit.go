package hardware

import (
	"image"
	"image/color"
	"image/draw"
	"reflect"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// overWhite[dst<<8|mask] is the gray value draw.DrawMask(draw.Over) leaves
// when compositing opaque white through an alpha mask value onto a gray
// pixel. It is filled by running draw.DrawMask itself on 1x1 images, so the
// fast blit below is pixel-identical to font.Drawer by construction.
var (
	overWhite     [256 * 256]byte
	overWhiteOnce sync.Once
)

func initOverWhite() {
	dst := image.NewGray(image.Rect(0, 0, 1, 1))
	mask := image.NewAlpha(image.Rect(0, 0, 1, 1))
	white := image.NewUniform(color.Gray{255})
	for d := 0; d < 256; d++ {
		for m := 0; m < 256; m++ {
			dst.Pix[0], mask.Pix[0] = byte(d), byte(m)
			draw.DrawMask(dst, dst.Rect, white, image.Point{}, mask, image.Point{}, draw.Over)
			overWhite[d<<8|m] = dst.Pix[0]
		}
	}
}

// drawString draws text in white on the canvas, as font.Drawer would with
// an image.Uniform white source, but blits cached *image.Alpha glyph masks
// with a table lookup. The generic draw.DrawMask path (gray destination,
// uniform source) converts every pixel through color.RGBA64, and opentype
// re-rasterizes each glyph on every call: together they were most of the
// cost of rendering the idle panel at 10Hz.
func (d *TTFDisplay) drawString(dot fixed.Point26_6, text string) {
	overWhiteOnce.Do(initOverWhite)
	white := image.NewUniform(color.Gray{255})
	face := faceID(d.font)
	prev := rune(-1)
	for _, c := range text {
		if prev >= 0 {
			dot.X += d.font.Kern(prev, c)
		}
		g, ok := d.glyph(face, dot, c)
		switch {
		case !ok:
			// Uncacheable face or mask type: draw it the generic way.
			dr, mask, mp, advance, _ := d.font.Glyph(dot, c)
			if !dr.Empty() {
				draw.DrawMask(d.canvas, dr, white, image.Point{}, mask, mp, draw.Over)
			}
			g.advance = advance
		case g.mask != nil:
			origin := image.Pt(dot.X.Floor(), dot.Y.Floor())
			d.blitWhite(g.dr.Add(origin), g.mask, image.Point{})
		}
		dot.X += g.advance
		prev = c
	}
}

// glyphKey identifies a rasterized glyph: opentype output depends on the
// face, the rune and the dot's sub-pixel phase; the integer part of the dot
// only translates it.
type glyphKey struct {
	face   int
	r      rune
	fx, fy int8
}

type cachedGlyph struct {
	dr      image.Rectangle // relative to the dot's integer position
	mask    *image.Alpha    // nil for an empty glyph (a space)
	advance fixed.Int26_6
}

// glyphCacheMax bounds the cache. The panel draws a small character set at
// a handful of sizes, so a full cache means something unusual (a long run
// of distinct text); it is simply dropped and refilled.
const glyphCacheMax = 4096

// glyph returns the cached rendering of r at dot, rasterizing it on a miss.
// ok is false when the face or its mask type can't be cached.
func (d *TTFDisplay) glyph(face int, dot fixed.Point26_6, r rune) (g cachedGlyph, ok bool) {
	if face == 0 {
		return g, false
	}
	key := glyphKey{face, r, int8(dot.X & 63), int8(dot.Y & 63)}
	if g, ok := d.glyphs[key]; ok {
		return g, true
	}
	dr, mask, mp, advance, _ := d.font.Glyph(dot, r)
	g.advance = advance
	origin := image.Pt(dot.X.Floor(), dot.Y.Floor())
	g.dr = dr.Sub(origin)
	if !dr.Empty() {
		a, isAlpha := mask.(*image.Alpha)
		if !isAlpha {
			return g, false
		}
		// The face reuses its mask buffer on the next call: copy.
		g.mask = image.NewAlpha(image.Rect(0, 0, dr.Dx(), dr.Dy()))
		for y := 0; y < dr.Dy(); y++ {
			if !image.Pt(mp.X, mp.Y+y).In(a.Rect) {
				continue
			}
			src := a.Pix[a.PixOffset(mp.X, mp.Y+y):]
			src = src[:min(len(src), dr.Dx(), a.Rect.Max.X-mp.X)]
			copy(g.mask.Pix[y*g.mask.Stride:], src)
		}
	}
	if d.glyphs == nil || len(d.glyphs) >= glyphCacheMax {
		d.glyphs = make(map[glyphKey]cachedGlyph)
		d.glyphFaces = make(map[int]font.Face)
	}
	d.glyphs[key] = g
	// Keys use the face's address: holding the face keeps that address
	// from being reused by another face while its glyphs are cached.
	d.glyphFaces[face] = d.font
	return g, true
}

// blitWhite composites white through mask (aligned so dr.Min matches mp)
// onto the canvas, clipped to both, like draw.DrawMask does.
func (d *TTFDisplay) blitWhite(dr image.Rectangle, mask *image.Alpha, mp image.Point) {
	clipped := dr.Intersect(d.canvas.Rect).Intersect(mask.Rect.Add(dr.Min.Sub(mp)))
	if clipped.Empty() {
		return
	}
	mp = mp.Add(clipped.Min.Sub(dr.Min))
	w := clipped.Dx()
	for y := 0; y < clipped.Dy(); y++ {
		do := d.canvas.PixOffset(clipped.Min.X, clipped.Min.Y+y)
		mo := mask.PixOffset(mp.X, mp.Y+y)
		drow := d.canvas.Pix[do : do+w]
		mrow := mask.Pix[mo : mo+w]
		for i, m := range mrow {
			switch m {
			case 0:
			case 255:
				drow[i] = 255
			default:
				drow[i] = overWhite[int(drow[i])<<8|int(m)]
			}
		}
	}
}

// faceID distinguishes font faces in the draw-call hash. Faces are pointer
// types in practice (opentype, basicfont); anything else hashes as 0, which
// at worst lets a face swap with identical calls skip one mirror reload.
func faceID(f font.Face) int {
	if v := reflect.ValueOf(f); v.Kind() == reflect.Pointer {
		return int(v.Pointer())
	}
	return 0
}
