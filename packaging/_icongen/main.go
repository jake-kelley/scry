// Command icongen draws scry's menu-bar template icon: a monochrome
// magnifying glass with an alpha channel, at 1024x1024. macOS recolours
// template icons for light/dark menu bars itself, so the source art must be
// plain black-on-transparent — no colour, no gradients, no drop shadow.
//
// This lives in packaging/_icongen (underscore prefix) specifically so the
// Go toolchain skips it during `go build ./...`, `go vet ./...`, and
// `go test ./...` — it is build tooling, not part of the scry module's
// package graph, and must never affect those commands.
//
// Regenerate with:
//
//	go run ./packaging/_icongen -out packaging/icon-source/icon-1024.png
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func main() {
	out := flag.String("out", "packaging/icon-source/icon-1024.png", "output PNG path")
	size := flag.Int("size", 1024, "image size in pixels (square)")
	flag.Parse()

	img := drawMagnifyingGlass(*size)

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "icongen:", err)
		os.Exit(1)
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		fmt.Fprintln(os.Stderr, "icongen:", err)
		os.Exit(1)
	}
}

// drawMagnifyingGlass renders a simple magnifying-glass silhouette:
// a ring plus a handle, black with per-pixel alpha (antialiased edges),
// everything else fully transparent. Proportions target macOS's ~60%
// content-in-frame convention for menu-bar template icons.
func drawMagnifyingGlass(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	center := float64(size) * 0.42
	radius := float64(size) * 0.26
	thickness := float64(size) * 0.075

	// Handle: a rotated capsule from the ring's rim out to the
	// bottom-right corner of the frame.
	handleAngle := math.Pi / 4 // 45 degrees, pointing down-right
	handleStart := radius - thickness*0.3
	handleLen := float64(size) * 0.34
	hx0 := center + math.Cos(handleAngle)*handleStart
	hy0 := center + math.Sin(handleAngle)*handleStart
	hx1 := center + math.Cos(handleAngle)*(handleStart+handleLen)
	hy1 := center + math.Sin(handleAngle)*(handleStart+handleLen)
	handleThickness := thickness * 0.9

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5

			// Distance from the ring's centreline.
			dx, dy := fx-center, fy-center
			ringDist := math.Abs(math.Hypot(dx, dy) - radius)
			ringAlpha := coverage(ringDist, thickness/2)

			// Distance from the handle segment.
			segDist := distToSegment(fx, fy, hx0, hy0, hx1, hy1)
			handleAlpha := coverage(segDist, handleThickness/2)

			a := math.Max(ringAlpha, handleAlpha)
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 0, A: uint8(a * 255)})
			}
		}
	}
	return img
}

// coverage turns a signed distance from a shape's edge into antialiased
// alpha coverage: 1 at the centre, 0 past halfWidth+1px, linear in between.
func coverage(dist, halfWidth float64) float64 {
	v := halfWidth - dist
	if v <= -1 {
		return 0
	}
	if v >= 1 {
		return 1
	}
	return (v + 1) / 2
}

// distToSegment returns the distance from point (px,py) to the line
// segment (x0,y0)-(x1,y1).
func distToSegment(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		return math.Hypot(px-x0, py-y0)
	}
	t := ((px-x0)*dx + (py-y0)*dy) / lenSq
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	cx, cy := x0+t*dx, y0+t*dy
	return math.Hypot(px-cx, py-cy)
}
