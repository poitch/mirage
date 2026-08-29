// Package preview makes the small images the mobile apps and the browser view
// show instead of a file icon.
//
// Previews are produced only when something asks for one, never in a sweep.
// Generating them for a share of three million files would mean reading every
// photograph on it, which is exactly the disk load the scanner is careful to
// avoid.
package preview

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"path"
	"strings"

	"github.com/poitch/mirage/internal/imgutil"
)

// Buckets are the sizes a preview is actually made at.
//
// Clients ask for arbitrary dimensions - a gallery asks for whatever its tile
// happens to be - and honouring each one exactly would mean a separate copy of
// every photograph for every layout anybody ever used. A request is rounded up
// to the next of these, so the cache stays a handful of entries per file.
var Buckets = []int{64, 128, 256, 512, 1024}

const (
	// MaxSize is the largest preview that will be made.
	MaxSize = 1024
	// MinSize is the smallest.
	MinSize = 32

	// maxPixels bounds what will be decoded. A small file can describe an
	// enormous image, and the allocation is the attack.
	maxPixels = 128 << 20

	// exifScanLimit is how far into a file to look for the EXIF block. It sits
	// near the front; reading further is a waste of a seek.
	exifScanLimit = 1 << 20

	// quality is the JPEG quality previews are encoded at. High enough that a
	// thumbnail does not look degraded, low enough to stay small.
	quality = 82
)

// ErrUnsupported is returned for a file no preview can be made from. It is not
// a failure - most files are not pictures - and clients answer it by showing an
// icon.
var ErrUnsupported = errors.New("no preview can be made for this file")

// Result is a generated preview.
type Result struct {
	Data          []byte
	Width, Height int
	// FromThumbnail records that the camera's own embedded thumbnail was used
	// rather than the full image being decoded. Worth knowing: it is the
	// difference between reading a few kilobytes and tens of megabytes.
	FromThumbnail bool
}

// Bucket rounds a requested size up to one that is actually made.
func Bucket(size int) int {
	for _, b := range Buckets {
		if size <= b {
			return b
		}
	}
	return MaxSize
}

// Supported reports whether a name looks like something a preview can be made
// from, without opening it.
//
// Checked by extension because the alternative is opening every file in a
// listing to find out, and on a share this size that is the whole cost the
// preview endpoint is trying to avoid.
func Supported(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif":
		return true
	}
	return false
}

// Generate makes a preview no larger than size on its longest side.
//
// r is read more than once, so it must seek. The file is opened, not the index:
// a preview of what the index remembers would be a preview of the wrong thing
// after somebody edits a photograph.
func Generate(r io.ReadSeeker, name string, size int) (Result, error) {
	if !Supported(name) {
		return Result{}, ErrUnsupported
	}
	size = min(max(size, MinSize), MaxSize)

	// The camera's own thumbnail first. It is a few kilobytes near the front of
	// the file, where decoding the picture means reading all of it and
	// expanding it in memory - on a spinning disk that is the whole cost.
	if img, ok := thumbnailFor(r, name, size); ok {
		return encode(img, true)
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return Result{}, ErrUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return Result{}, ErrUnsupported
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	src, _, err := image.Decode(r)
	if err != nil {
		return Result{}, ErrUnsupported
	}

	img := imgutil.Fit(imgutil.ToRGBA(src), size, size)
	// Applied after scaling, which is cheaper: the rotation then moves a
	// thumbnail's worth of pixels rather than the whole photograph's.
	if o := orientationOf(r, name); o != OrientationNormal {
		img = applyOrientation(img, o)
	}
	return encode(img, false)
}

// thumbnailFor returns the camera's embedded thumbnail, if there is one and it
// is large enough to serve the requested size.
//
// Large enough matters: a typical embedded thumbnail is 160x120, so it answers
// a request for a small tile perfectly and would have to be blown up for
// anything bigger, which looks worse than the icon it replaced.
func thumbnailFor(r io.ReadSeeker, name string, size int) (*image.RGBA, bool) {
	exif, block, err := readEXIF(r, name)
	if err != nil {
		return nil, false
	}
	thumb := exif.thumbnail(block)
	if thumb == nil {
		return nil, false
	}
	src, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		return nil, false
	}
	b := src.Bounds()
	if b.Dx() < size && b.Dy() < size {
		return nil, false
	}

	img := imgutil.Fit(imgutil.ToRGBA(src), size, size)
	if exif.Orientation != OrientationNormal {
		img = applyOrientation(img, exif.Orientation)
	}
	return img, true
}

// orientationOf reads just the orientation, for the path where the picture was
// decoded in full.
func orientationOf(r io.ReadSeeker, name string) Orientation {
	exif, _, err := readEXIF(r, name)
	if err != nil {
		return OrientationNormal
	}
	return exif.Orientation
}

// readEXIF finds and parses a JPEG's EXIF block.
func readEXIF(r io.ReadSeeker, name string) (exifData, []byte, error) {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg":
	default:
		return exifData{}, nil, errNoEXIF
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return exifData{}, nil, err
	}
	head, err := io.ReadAll(io.LimitReader(r, exifScanLimit))
	if err != nil {
		return exifData{}, nil, err
	}
	block, err := findEXIFBlock(head)
	if err != nil {
		return exifData{}, nil, err
	}
	data, err := parseEXIF(block)
	return data, block, err
}

// findEXIFBlock walks a JPEG's segment headers to the APP1 segment holding
// EXIF.
//
// Walked rather than searched for, because the bytes "Exif\0\0" can occur
// inside image data and a search would find one and read nonsense.
func findEXIFBlock(b []byte) ([]byte, error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errNoEXIF
	}
	pos := 2
	for pos+4 <= len(b) {
		if b[pos] != 0xFF {
			return nil, errNoEXIF
		}
		marker := b[pos+1]
		// Start of scan: the image data begins and there are no more headers.
		if marker == 0xDA {
			return nil, errNoEXIF
		}
		length := int(b[pos+2])<<8 | int(b[pos+3])
		if length < 2 || pos+2+length > len(b) {
			return nil, errNoEXIF
		}
		if marker == 0xE1 {
			seg := b[pos+4 : pos+2+length]
			if header := []byte("Exif\x00\x00"); len(seg) > len(header) &&
				bytes.Equal(seg[:len(header)], header) {
				return seg[len(header):], nil
			}
		}
		pos += 2 + length
	}
	return nil, errNoEXIF
}

// applyOrientation turns an image the way EXIF says it should be shown.
func applyOrientation(src *image.RGBA, o Orientation) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	// The four rotated orientations swap the axes.
	outW, outH := w, h
	switch o {
	case 5, 6, 7, 8:
		outW, outH = h, w
	}

	out := image.NewRGBA(image.Rect(0, 0, outW, outH))
	for y := range h {
		for x := range w {
			var dx, dy int
			switch o {
			case 2: // mirrored
				dx, dy = w-1-x, y
			case 3: // rotated 180
				dx, dy = w-1-x, h-1-y
			case 4: // mirrored vertically
				dx, dy = x, h-1-y
			case 5: // mirrored and rotated 90 clockwise
				dx, dy = y, x
			case 6: // rotated 90 clockwise
				dx, dy = h-1-y, x
			case 7: // mirrored and rotated 90 anticlockwise
				dx, dy = h-1-y, w-1-x
			case 8: // rotated 90 anticlockwise
				dx, dy = y, w-1-x
			default:
				dx, dy = x, y
			}
			si := src.PixOffset(b.Min.X+x, b.Min.Y+y)
			di := out.PixOffset(dx, dy)
			copy(out.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return out
}

func encode(img *image.RGBA, fromThumbnail bool) (Result, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return Result{}, fmt.Errorf("encode preview: %w", err)
	}
	b := img.Bounds()
	return Result{
		Data: buf.Bytes(), Width: b.Dx(), Height: b.Dy(), FromThumbnail: fromThumbnail,
	}, nil
}

// Keep the decoders registered for image.Decode.
var (
	_ = png.Decode
	_ = gif.Decode
)
