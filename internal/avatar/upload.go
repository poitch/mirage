package avatar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"  // decode uploaded GIFs
	_ "image/jpeg" // decode uploaded JPEGs
	"image/png"
	"time"
)

const (
	// Canonical is the size an uploaded picture is stored at. Every size a
	// client asks for is derived from this one, so it is the largest that will
	// ever be served at full detail. Clients ask for at most 512.
	Canonical = 512

	// MaxUploadBytes bounds what may be accepted. A picture large enough to
	// need more than this is one the person did not mean to upload.
	MaxUploadBytes = 8 << 20

	// maxSourcePixels bounds the decoded image. A small file can decode to an
	// enormous one, so the dimensions are checked from the header before any
	// of it is decoded.
	maxSourcePixels = 64 << 20
)

// ErrUnsupportedImage is returned for a file that is not a picture Mirage can
// read.
var ErrUnsupportedImage = errors.New("that file is not a PNG, JPEG or GIF image")

// Normalise turns an uploaded file into the form stored for an account: square,
// no larger than Canonical, encoded as PNG.
//
// Cropping rather than squashing is deliberate. Clients render avatars square
// or circular and would distort anything else, and a person who uploads a
// portrait means the middle of it.
func Normalise(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, ErrUnsupportedImage
	}
	if len(data) > MaxUploadBytes {
		return nil, fmt.Errorf("that image is %s; the limit is %s",
			humanBytes(len(data)), humanBytes(MaxUploadBytes))
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrUnsupportedImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, ErrUnsupportedImage
	}
	// Checked before decoding: a few hundred kilobytes of PNG can expand into
	// gigabytes of pixels, and that allocation is the attack.
	if int64(cfg.Width)*int64(cfg.Height) > maxSourcePixels {
		return nil, fmt.Errorf("that image is %dx%d, which is larger than Mirage will decode",
			cfg.Width, cfg.Height)
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrUnsupportedImage
	}

	square := cropSquare(toRGBA(src))
	size := min(square.Bounds().Dx(), Canonical)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, scale(square, size)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Render produces a stored picture at the size a client asked for.
func Render(stored []byte, size int) ([]byte, error) {
	size = clamp(size, MinSize, MaxSize)

	src, err := png.Decode(bytes.NewReader(stored))
	if err != nil {
		return nil, err
	}
	img := toRGBA(src)
	if img.Bounds().Dx() == size {
		return stored, nil
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, scale(img, size)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UploadedETag identifies a stored picture at a size, so a client holding it
// can be answered with a 304. The version changes whenever the picture does.
func UploadedETag(seed string, updated time.Time, size int) string {
	h := sha256.New()
	h.Write([]byte(seed))
	fmt.Fprintf(h, "|%d|%d|%d", updated.UnixNano(), clamp(size, MinSize, MaxSize), version)
	return `"` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
}

// toRGBA converts any image to RGBA, whose colours are premultiplied by alpha -
// which is what makes averaging pixels during scaling correct rather than
// leaving light fringes around anything transparent.
func toRGBA(src image.Image) *image.RGBA {
	if out, ok := src.(*image.RGBA); ok {
		return out
	}
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

// cropSquare takes the largest centred square from an image.
func cropSquare(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2

	out := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(out, out.Bounds(), src, image.Pt(x0, y0), draw.Src)
	return out
}

// scale resizes a square image.
//
// Shrinking averages every source pixel that falls under each destination one,
// so detail is merged rather than sampled - a photograph reduced to 64 pixels
// by picking one pixel in eight looks like noise. Growing interpolates between
// neighbours instead, since there is nothing to average.
func scale(src *image.RGBA, size int) *image.RGBA {
	b := src.Bounds()
	if b.Dx() == size && b.Dy() == size {
		return src
	}
	if size < b.Dx() {
		return shrink(src, size)
	}
	return grow(src, size)
}

func shrink(src *image.RGBA, size int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, size, size))

	for y := range size {
		y0 := b.Min.Y + y*sh/size
		y1 := max(b.Min.Y+(y+1)*sh/size, y0+1)
		for x := range size {
			x0 := b.Min.X + x*sw/size
			x1 := max(b.Min.X+(x+1)*sw/size, x0+1)

			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					i := src.PixOffset(sx, sy)
					p := src.Pix[i : i+4 : i+4]
					r += uint32(p[0])
					g += uint32(p[1])
					bl += uint32(p[2])
					a += uint32(p[3])
					n++
				}
			}
			i := out.PixOffset(x, y)
			out.Pix[i+0] = uint8(r / n)
			out.Pix[i+1] = uint8(g / n)
			out.Pix[i+2] = uint8(bl / n)
			out.Pix[i+3] = uint8(a / n)
		}
	}
	return out
}

func grow(src *image.RGBA, size int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, size, size))

	for y := range size {
		fy := (float64(y) + 0.5) * float64(sh) / float64(size)
		y0, wy := interpolate(fy, sh)
		for x := range size {
			fx := (float64(x) + 0.5) * float64(sw) / float64(size)
			x0, wx := interpolate(fx, sw)

			i := out.PixOffset(x, y)
			for c := range 4 {
				tl := float64(src.Pix[src.PixOffset(b.Min.X+x0, b.Min.Y+y0)+c])
				tr := float64(src.Pix[src.PixOffset(b.Min.X+min(x0+1, sw-1), b.Min.Y+y0)+c])
				bl := float64(src.Pix[src.PixOffset(b.Min.X+x0, b.Min.Y+min(y0+1, sh-1))+c])
				br := float64(src.Pix[src.PixOffset(b.Min.X+min(x0+1, sw-1), b.Min.Y+min(y0+1, sh-1))+c])
				top := tl + (tr-tl)*wx
				bottom := bl + (br-bl)*wx
				out.Pix[i+c] = uint8(top + (bottom-top)*wy + 0.5)
			}
		}
	}
	return out
}

// interpolate splits a source coordinate into the pixel to its left and how far
// past it the coordinate lies.
func interpolate(f float64, limit int) (int, float64) {
	f -= 0.5
	if f < 0 {
		return 0, 0
	}
	i := int(f)
	if i >= limit-1 {
		return limit - 1, 0
	}
	return i, f - float64(i)
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
