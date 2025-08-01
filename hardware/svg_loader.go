package hardware

import (
	"bytes"
	"fmt"
	"image"
	"os"
	"path/filepath"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

// SVGLoader handles loading and converting SVG files to bitmap data
type SVGLoader struct {
	svgDir string
}

// NewSVGLoader creates a new SVG loader with the specified directory
func NewSVGLoader(svgDir string) *SVGLoader {
	return &SVGLoader{
		svgDir: svgDir,
	}
}

// LoadSVGAsBitmap loads an SVG file and converts it to a bitmap array
func (sl *SVGLoader) LoadSVGAsBitmap(filename string, size int) ([][]byte, error) {
	// Construct the full path to the SVG file
	svgPath := filepath.Join(sl.svgDir, filename)
	
	// Read the SVG file
	svgData, err := os.ReadFile(svgPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read SVG file %s: %w", svgPath, err)
	}

	// Parse the SVG
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svgData))
	if err != nil {
		return nil, fmt.Errorf("failed to parse SVG: %w", err)
	}

	// Create a raster image
	w, h := size, size
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	
	// Create scanner and rasterize the SVG
	scanner := rasterx.NewScannerGV(w, h, img, img.Bounds())
	raster := rasterx.NewDasher(w, h, scanner)
	
	// Set the viewbox to fit the target size
	icon.SetTarget(0, 0, float64(w), float64(h))
	icon.Draw(raster, 1.0)

	// Convert to our bitmap format (grayscale byte array)
	bitmap := make([][]byte, size)
	for y := 0; y < size; y++ {
		bitmap[y] = make([]byte, size)
		for x := 0; x < size; x++ {
			// Get the pixel color
			c := img.RGBAAt(x, y)
			
			// Convert to grayscale and determine if pixel should be "on"
			// Use the alpha channel to determine visibility
			if c.A > 128 {
				// Convert RGB to grayscale
				gray := uint8((int(c.R)*299 + int(c.G)*587 + int(c.B)*114) / 1000)
				// If the pixel is dark enough (considering it's likely black on transparent)
				// we'll make it visible with intensity 15 (max brightness for our display)
				if gray < 128 {
					bitmap[y][x] = 15
				} else {
					bitmap[y][x] = 0
				}
			} else {
				bitmap[y][x] = 0
			}
		}
	}

	return bitmap, nil
}

// LoadUSBIcon loads the USB SVG icon as bitmap data
func (sl *SVGLoader) LoadUSBIcon(size int, useSmall bool) ([][]byte, error) {
	targetSize := size
	if useSmall {
		targetSize = 8
	} else {
		targetSize = 16
	}
	
	return sl.LoadSVGAsBitmap("usb.svg", targetSize)
}

// LoadNetworkIcon loads the network SVG icon as bitmap data
func (sl *SVGLoader) LoadNetworkIcon(size int, useSmall bool) ([][]byte, error) {
	targetSize := size
	if useSmall {
		targetSize = 8
	} else {
		targetSize = 16
	}
	
	return sl.LoadSVGAsBitmap("network.svg", targetSize)
}

// ConvertToFixedArray16 converts a dynamic bitmap to a fixed 16x16 array
func ConvertToFixedArray16(bitmap [][]byte) [16][16]byte {
	var result [16][16]byte
	
	size := len(bitmap)
	if size > 16 {
		size = 16
	}
	
	for y := 0; y < size; y++ {
		rowSize := len(bitmap[y])
		if rowSize > 16 {
			rowSize = 16
		}
		for x := 0; x < rowSize; x++ {
			result[y][x] = bitmap[y][x]
		}
	}
	
	return result
}

// ConvertToFixedArray8 converts a dynamic bitmap to a fixed 8x8 array
func ConvertToFixedArray8(bitmap [][]byte) [8][8]byte {
	var result [8][8]byte
	
	size := len(bitmap)
	if size > 8 {
		size = 8
	}
	
	for y := 0; y < size; y++ {
		rowSize := len(bitmap[y])
		if rowSize > 8 {
			rowSize = 8
		}
		for x := 0; x < rowSize; x++ {
			result[y][x] = bitmap[y][x]
		}
	}
	
	return result
}