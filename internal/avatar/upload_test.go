package avatar

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

// timeAt builds a distinct picture version.
func timeAt(n int64) time.Time { return time.Unix(0, n) }

// testImage builds a picture whose left half is red and right half is blue, so
// that cropping and scaling can be checked by looking at the colours.
func testImage(t *testing.T, w, h int) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{R: 0xff, A: 0xff}
			if x >= w/2 {
				c = color.RGBA{B: 0xff, A: 0xff}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

func TestNormaliseProducesASquareWithinTheCanonicalSize(t *testing.T) {
	for _, tc := range []struct{ w, h, want int }{
		{1000, 800, Canonical}, // shrunk to the cap
		{800, 1000, Canonical},
		{300, 300, 300}, // already small; not enlarged
		{400, 200, 200}, // the short side sets the square
	} {
		out, err := Normalise(encodePNG(t, testImage(t, tc.w, tc.h)))
		if err != nil {
			t.Fatalf("Normalise(%dx%d): %v", tc.w, tc.h, err)
		}
		b := decode(t, out).Bounds()
		if b.Dx() != b.Dy() {
			t.Errorf("%dx%d produced %dx%d, which is not square", tc.w, tc.h, b.Dx(), b.Dy())
		}
		if b.Dx() != tc.want {
			t.Errorf("%dx%d produced %dpx, want %d", tc.w, tc.h, b.Dx(), tc.want)
		}
	}
}

// TestNormaliseCropsFromTheCentre: a person uploading a portrait means the
// middle of it, and squashing it to a square would distort their face.
func TestNormaliseCropsFromTheCentre(t *testing.T) {
	// A wide image: the left third red, then blue. Cropping centrally keeps the
	// middle, so the result's left edge should be blue, not red.
	src := image.NewRGBA(image.Rect(0, 0, 300, 100))
	for y := range 100 {
		for x := range 300 {
			c := color.RGBA{B: 0xff, A: 0xff}
			if x < 100 {
				c = color.RGBA{R: 0xff, A: 0xff}
			}
			src.Set(x, y, c)
		}
	}
	out, err := Normalise(encodePNG(t, src))
	if err != nil {
		t.Fatalf("Normalise: %v", err)
	}
	got := decode(t, out)
	r, _, b, _ := got.At(2, 50).RGBA()
	if r > b {
		t.Errorf("the crop kept the left edge instead of the centre: got r=%d b=%d", r>>8, b>>8)
	}
}

func TestNormaliseAcceptsJPEG(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(t, 400, 400), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	if _, err := Normalise(buf.Bytes()); err != nil {
		t.Errorf("Normalise rejected a JPEG: %v", err)
	}
}

func TestNormaliseRejectsWhatItCannotRead(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     {},
		"text":      []byte("this is not a picture"),
		"truncated": encodePNG(t, testImage(t, 64, 64))[:20],
	} {
		if _, err := Normalise(data); err == nil {
			t.Errorf("Normalise accepted %s", name)
		}
	}
}

func TestNormaliseRejectsOversizedUploads(t *testing.T) {
	_, err := Normalise(make([]byte, MaxUploadBytes+1))
	if err == nil {
		t.Fatal("Normalise accepted a file over the limit")
	}
	// The message has to say what the limit is, or the person has no idea what
	// to do about it.
	if !bytes.Contains([]byte(err.Error()), []byte("limit")) {
		t.Errorf("error %q does not mention the limit", err)
	}
}

// TestNormaliseShrinksByAveraging: reducing a photograph by picking one pixel
// in eight turns it into noise, so the pixels under each destination one are
// merged instead.
func TestNormaliseShrinksByAveraging(t *testing.T) {
	// Alternating black and white columns. Sampling would give solid black or
	// solid white; averaging gives grey.
	src := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := range 512 {
		for x := range 512 {
			c := color.RGBA{A: 0xff}
			if x%2 == 0 {
				c = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
			}
			src.Set(x, y, c)
		}
	}
	out, err := Render(encodePNG(t, src), 64)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	r, _, _, _ := decode(t, out).At(32, 32).RGBA()
	if v := r >> 8; v < 0x60 || v > 0xa0 {
		t.Errorf("a shrunk checkerboard gave %#02x, want mid grey: the pixels were sampled, not averaged", v)
	}
}

func TestRenderProducesTheRequestedSize(t *testing.T) {
	stored, err := Normalise(encodePNG(t, testImage(t, 512, 512)))
	if err != nil {
		t.Fatalf("Normalise: %v", err)
	}
	for _, size := range []int{16, 64, 128, 512, 1024} {
		out, err := Render(stored, size)
		if err != nil {
			t.Fatalf("Render(%d): %v", size, err)
		}
		if got := decode(t, out).Bounds().Dx(); got != size {
			t.Errorf("Render(%d) produced %dpx", size, got)
		}
	}
}

func TestUploadedETagChangesWithThePicture(t *testing.T) {
	early := UploadedETag("1:alice", timeAt(1), 128)
	if early != UploadedETag("1:alice", timeAt(1), 128) {
		t.Error("UploadedETag is not stable for the same picture")
	}
	if early == UploadedETag("1:alice", timeAt(2), 128) {
		t.Error("uploading a new picture did not change the ETag; clients would keep the old one")
	}
	if early == UploadedETag("1:alice", timeAt(1), 512) {
		t.Error("two sizes share an ETag")
	}
}
