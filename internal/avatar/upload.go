package avatar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // decode uploaded GIFs
	_ "image/jpeg" // decode uploaded JPEGs
	"image/png"

	"github.com/poitch/mirage/internal/imgutil"
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

	square := imgutil.CropSquare(imgutil.ToRGBA(src))
	size := min(square.Bounds().Dx(), Canonical)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, imgutil.Scale(square, size, size)); err != nil {
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
	img := imgutil.ToRGBA(src)
	if img.Bounds().Dx() == size {
		return stored, nil
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, imgutil.Scale(img, size, size)); err != nil {
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
