// Package avatar draws the small identity images the sync clients ask for.
//
// Nextcloud lets people upload a picture and falls back to a generated one when
// they have not. Mirage has no upload path, so every avatar is generated: a
// symmetric pattern and a colour derived from the username, stable for as long
// as the account exists. That is enough for the thing an avatar is actually for
// here - telling one account apart from another at a glance - without pulling
// in a font or storing image data.
package avatar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"math"
)

const (
	// MinSize and MaxSize bound what a client may ask for. The clients use 64,
	// 128 and 512; the bounds only exist so a request cannot ask for an image
	// large enough to be worth allocating.
	MinSize = 8
	MaxSize = 1024

	// DefaultSize is used when a request does not name one.
	DefaultSize = 128

	// grid is the width of the pattern in cells. Only the left half plus the
	// centre column is drawn; the rest is mirrored, which is what makes the
	// result read as a face-like mark rather than as noise.
	grid = 5

	// version changes whenever the drawing changes, so that a client holding a
	// cached copy under the old ETag fetches the new one.
	version = 1
)

// Generate draws the avatar for seed at the given size and encodes it as PNG.
//
// size is clamped rather than rejected: a client asking for an odd size wants a
// picture, not an error, and the caller has no better answer than the nearest
// one that fits.
func Generate(seed string, size int) ([]byte, error) {
	size = clamp(size, MinSize, MaxSize)
	sum := sha256.Sum256([]byte(seed))

	fg := hashColor(sum)
	bg := color.NRGBA{R: 0xf2, G: 0xf2, B: 0xf4, A: 0xff}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	fill(img, image.Rect(0, 0, size, size), bg)

	// A margin keeps the pattern clear of the edge, which matters because most
	// clients round the corners or mask it to a circle.
	margin := size / 8
	inner := size - 2*margin
	half := grid/2 + 1

	for col := range half {
		for row := range grid {
			if sum[col*grid+row]&1 == 0 {
				continue
			}
			r := cellRect(margin, inner, col, row)
			fill(img, r, fg)
			fill(img, mirrorX(r, size), fg)
		}
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ETag identifies the image a seed and size produce, so a client that already
// has it can be answered with a 304 rather than the bytes.
func ETag(seed string, size int) string {
	size = clamp(size, MinSize, MaxSize)
	sum := sha256.Sum256([]byte(seed))
	h := sha256.New()
	h.Write(sum[:])
	h.Write([]byte{byte(version), byte(size), byte(size >> 8)})
	return `"` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
}

// cellRect is the pixel rectangle for one cell of the grid.
//
// The edges are computed from the cell index rather than by multiplying out a
// cell size, so that rounding is spread across the image instead of leaving a
// gap down one side when inner does not divide by grid.
func cellRect(margin, inner, col, row int) image.Rectangle {
	x0 := margin + col*inner/grid
	x1 := margin + (col+1)*inner/grid
	y0 := margin + row*inner/grid
	y1 := margin + (row+1)*inner/grid
	return image.Rect(x0, y0, x1, y1)
}

// fill paints a rectangle a single colour.
func fill(img *image.NRGBA, r image.Rectangle, c color.NRGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

// hashColor picks a colour from the hash.
//
// The hue is taken from the hash but saturation and lightness are fixed, so
// every account gets a distinct colour and none of them comes out muddy or so
// pale that the pattern disappears.
func hashColor(sum [32]byte) color.NRGBA {
	hue := float64(uint16(sum[0])<<8|uint16(sum[1])) / 65535 * 360
	r, g, b := hslToRGB(hue, 0.62, 0.45)
	return color.NRGBA{R: r, G: g, B: b, A: 0xff}
}

func hslToRGB(h, s, l float64) (uint8, uint8, uint8) {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2

	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	to8 := func(v float64) uint8 { return uint8(math.Round((v + m) * 255)) }
	return to8(r), to8(g), to8(b)
}

// mirrorX reflects a rectangle about the vertical centre line.
//
// The mirroring is done on pixels rather than on cell indices because the cell
// edges are not evenly spaced - inner rarely divides by grid - so mirroring the
// index would draw the reflected column a pixel wide in places.
func mirrorX(r image.Rectangle, size int) image.Rectangle {
	return image.Rect(size-r.Max.X, r.Min.Y, size-r.Min.X, r.Max.Y)
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
