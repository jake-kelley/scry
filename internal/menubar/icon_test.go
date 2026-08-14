package menubar

import (
	"bytes"
	"image/png"
	"testing"
)

// TestTemplateIconPNG checks the property §7 actually cares about: the
// icon decodes as a valid PNG, has a non-trivial alpha channel (some
// transparent pixels, some opaque — not a solid block), and every opaque
// pixel is pure black, never colored. A colored template icon "looks
// broken in one of light/dark and you only notice after shipping" — this
// test is what would have caught that before shipping.
func TestTemplateIconPNG(t *testing.T) {
	data := templateIconPNG()
	if len(data) == 0 {
		t.Fatal("templateIconPNG() returned no data")
	}

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("templateIconPNG() did not decode as PNG: %v", err)
	}

	bounds := img.Bounds()
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		t.Fatalf("templateIconPNG() decoded to an empty image: %v", bounds)
	}

	var sawOpaque, sawTransparent bool
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			switch a {
			case 0:
				sawTransparent = true
			case 0xffff:
				sawOpaque = true
				if r != 0 || g != 0 || b != 0 {
					t.Fatalf("pixel (%d,%d) is opaque but not black: rgba=%d,%d,%d,%d", x, y, r, g, b, a)
				}
			default:
				t.Fatalf("pixel (%d,%d) has partial alpha %d, want fully opaque or fully transparent (template icons are strictly monochrome+alpha)", x, y, a)
			}
		}
	}
	if !sawOpaque {
		t.Error("templateIconPNG() has no opaque pixels — icon would be invisible")
	}
	if !sawTransparent {
		t.Error("templateIconPNG() has no transparent pixels — not actually using the alpha channel")
	}
}

func TestTemplateIconPNGDeterministic(t *testing.T) {
	a := templateIconPNG()
	b := templateIconPNG()
	if !bytes.Equal(a, b) {
		t.Error("templateIconPNG() is not deterministic across calls")
	}
}
