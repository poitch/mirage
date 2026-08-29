// Package imgutil holds the image operations shared by the account pictures and
// the file previews.
//
// Both need the same thing: take an image somebody else produced and make a
// small one out of it that still looks like the original. The scaling is the
// interesting part, and it is written out here rather than pulled in because
// the whole of it is a page of code and a dependency would be larger than that.
package imgutil

import (
	"image"
	"image/draw"
)

// ToRGBA converts any image to RGBA.
//
// RGBA's colours are premultiplied by alpha, which is what makes averaging
// pixels during scaling correct rather than leaving light fringes around
// anything transparent.
func ToRGBA(src image.Image) *image.RGBA {
	if out, ok := src.(*image.RGBA); ok {
		return out
	}
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

// CropSquare takes the largest centred square from an image.
func CropSquare(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2

	out := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(out, out.Bounds(), src, image.Pt(x0, y0), draw.Src)
	return out
}

// FitDimensions reports the size an image should be scaled to so that it fits
// inside maxW by maxH without distorting it, and without enlarging it.
//
// Not enlarging is deliberate: a client asking for a 1024px preview of a 300px
// image wants the image, and blowing it up only wastes bytes to make it blurry.
func FitDimensions(w, h, maxW, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if w <= maxW && h <= maxH {
		return w, h
	}
	// Whichever axis is furthest over decides the scale, so the other one
	// necessarily lands inside the box.
	byWidth := float64(maxW) / float64(w)
	byHeight := float64(maxH) / float64(h)
	scale := min(byWidth, byHeight)

	return max(int(float64(w)*scale+0.5), 1), max(int(float64(h)*scale+0.5), 1)
}

// Scale resizes an image to exactly w by h.
//
// Shrinking averages every source pixel that falls under each destination one,
// so detail is merged rather than sampled - a photograph reduced to 64 pixels
// by picking one pixel in eight looks like noise. Growing interpolates between
// neighbours instead, since there is nothing to average.
func Scale(src *image.RGBA, w, h int) *image.RGBA {
	b := src.Bounds()
	if b.Dx() == w && b.Dy() == h {
		return src
	}
	if w <= b.Dx() && h <= b.Dy() {
		return shrink(src, w, h)
	}
	return grow(src, w, h)
}

// Fit scales an image down to fit inside maxW by maxH, preserving its shape.
func Fit(src *image.RGBA, maxW, maxH int) *image.RGBA {
	b := src.Bounds()
	w, h := FitDimensions(b.Dx(), b.Dy(), maxW, maxH)
	if w == 0 || h == 0 {
		return src
	}
	return Scale(src, w, h)
}

func shrink(src *image.RGBA, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		y0 := b.Min.Y + y*sh/h
		y1 := max(b.Min.Y+(y+1)*sh/h, y0+1)
		for x := range w {
			x0 := b.Min.X + x*sw/w
			x1 := max(b.Min.X+(x+1)*sw/w, x0+1)

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

func grow(src *image.RGBA, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := range h {
		fy := (float64(y) + 0.5) * float64(sh) / float64(h)
		y0, wy := interpolate(fy, sh)
		for x := range w {
			fx := (float64(x) + 0.5) * float64(sw) / float64(w)
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
