package menubar

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// templateIconPNG renders the status item's icon: a plain magnifying
// glass, opaque black on a transparent background, nothing else. §7 is
// explicit about the trap here — "use a template icon: a monochrome
// image with an alpha channel, which macOS recolours for light and dark
// menu bars. A coloured PNG looks broken in one of the two, and you will
// only notice after shipping" — so the icon is generated in Go with
// image/draw rather than shipped as a static asset, which makes
// "monochrome + alpha, nothing else" a property tests can actually check
// (see TestTemplateIconPNG) instead of trusting whoever last exported a
// PNG in an image editor.
func templateIconPNG() []byte {
	const size = 22 // matches macOS's standard menu bar icon height

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	// Fully transparent background; draw sets only the glyph's pixels.
	draw.Draw(img, img.Bounds(), image.Transparent, image.Point{}, draw.Src)

	black := color.NRGBA{A: 255} // opaque black; macOS supplies the actual color

	// The lens: a ring, not a filled disk, so it reads as a magnifying
	// glass rather than a dot.
	cx, cy, rOuter, rInner := 8, 8, 6, 4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-cx, y-cy
			d2 := dx*dx + dy*dy
			if d2 <= rOuter*rOuter && d2 >= rInner*rInner {
				img.SetNRGBA(x, y, black)
			}
		}
	}

	// The handle: a short diagonal stroke from the lens's lower-right
	// edge toward the icon's corner.
	hx, hy := cx+rOuter-1, cy+rOuter-1
	for i := 0; i < 7; i++ {
		x, y := hx+i, hy+i
		if x < size && y < size {
			img.SetNRGBA(x, y, black)
			if x+1 < size {
				img.SetNRGBA(x+1, y, black) // thicken the stroke by one pixel
			}
		}
	}

	var buf bytes.Buffer
	// png.Encode on a fixed, hand-built image never fails; if it somehow
	// did, systray.SetTemplateIcon just gets an empty slice and shows no
	// icon rather than the process crashing over a decorative asset.
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
