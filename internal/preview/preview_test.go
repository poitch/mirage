package preview

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// A gradient rather than a flat colour, so scaling has something to average
	// and a bug that samples instead would show.
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{G: 0xc0, A: 0xff}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func TestGenerateFitsInsideTheRequestedBox(t *testing.T) {
	for _, tc := range []struct{ w, h, size, wantW, wantH int }{
		{800, 600, 256, 256, 192}, // landscape
		{600, 800, 256, 192, 256}, // portrait
		{500, 500, 256, 256, 256}, // square
		// Never enlarged: asking for a big preview of a small picture should
		// give the picture, not a blurry blow-up of it.
		{100, 80, 512, 100, 80},
	} {
		got, err := Generate(bytes.NewReader(makeJPEG(t, tc.w, tc.h)), "photo.jpg", tc.size)
		if err != nil {
			t.Fatalf("Generate(%dx%d, %d): %v", tc.w, tc.h, tc.size, err)
		}
		if got.Width != tc.wantW || got.Height != tc.wantH {
			t.Errorf("%dx%d at size %d gave %dx%d, want %dx%d",
				tc.w, tc.h, tc.size, got.Width, got.Height, tc.wantW, tc.wantH)
		}
		if _, err := jpeg.Decode(bytes.NewReader(got.Data)); err != nil {
			t.Errorf("the preview is not a decodable JPEG: %v", err)
		}
	}
}

func TestGeneratePNG(t *testing.T) {
	got, err := Generate(bytes.NewReader(makePNG(t, 400, 400)), "shot.png", 128)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Width != 128 || got.Height != 128 {
		t.Errorf("got %dx%d, want 128x128", got.Width, got.Height)
	}
}

// TestGenerateRefusesWhatItCannotRead: most files are not pictures, and that is
// not a failure - the client draws its own icon.
func TestGenerateRefusesWhatItCannotRead(t *testing.T) {
	for name, data := range map[string][]byte{
		"a document":        []byte("not a picture"),
		"a truncated JPEG":  makeJPEG(t, 100, 100)[:30],
		"an unknown format": makeJPEG(t, 100, 100),
	} {
		fileName := "notes.txt"
		if name == "a truncated JPEG" {
			fileName = "photo.jpg"
		}
		if _, err := Generate(bytes.NewReader(data), fileName, 128); err == nil {
			t.Errorf("Generate accepted %s", name)
		}
	}
}

func TestSupportedByExtension(t *testing.T) {
	for name, want := range map[string]bool{
		"a.jpg": true, "a.JPEG": true, "a.png": true, "a.gif": true,
		"a.heic": false, "a.txt": false, "a.mp4": false, "a": false,
	} {
		if got := Supported(name); got != want {
			t.Errorf("Supported(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestBucketRoundsUp(t *testing.T) {
	for ask, want := range map[int]int{
		1: 64, 64: 64, 65: 128, 200: 256, 256: 256, 700: 1024, 5000: 1024,
	} {
		if got := Bucket(ask); got != want {
			t.Errorf("Bucket(%d) = %d, want %d", ask, got, want)
		}
	}
}

// TestApplyOrientationTurnsThePicture: phones record how they were held rather
// than rotating the pixels, so without this every photo taken upright appears
// on its side.
func TestApplyOrientationTurnsThePicture(t *testing.T) {
	// A 2x1 image: left pixel red, right pixel blue.
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	src.Set(1, 0, color.RGBA{B: 0xff, A: 0xff})

	// Orientation 6 says the 0th column is the visual top, so rotating it for
	// display gives a 1x2 image with the red pixel at the top.
	got := applyOrientation(src, 6)
	if b := got.Bounds(); b.Dx() != 1 || b.Dy() != 2 {
		t.Fatalf("orientation 6 gave %dx%d, want 1x2", b.Dx(), b.Dy())
	}
	if r, _, _, _ := got.At(0, 0).RGBA(); r>>8 != 0xff {
		t.Errorf("orientation 6 did not put the red pixel at the top")
	}
	if _, _, b, _ := got.At(0, 1).RGBA(); b>>8 != 0xff {
		t.Errorf("orientation 6 did not put the blue pixel at the bottom")
	}

	// Rotated 180: same shape, ends swapped.
	flipped := applyOrientation(src, 3)
	if r, _, _, _ := flipped.At(1, 0).RGBA(); r>>8 != 0xff {
		t.Errorf("orientation 3 did not swap the pixels")
	}

	// Normal is left alone.
	same := applyOrientation(src, OrientationNormal)
	if r, _, _, _ := same.At(0, 0).RGBA(); r>>8 != 0xff {
		t.Errorf("orientation 1 changed the picture")
	}
}

func TestFindEXIFBlockWalksSegments(t *testing.T) {
	// Not a JPEG at all.
	if _, err := findEXIFBlock([]byte("hello")); err == nil {
		t.Error("findEXIFBlock accepted something that is not a JPEG")
	}
	// A JPEG with no EXIF segment.
	if _, err := findEXIFBlock(makeJPEG(t, 20, 20)); err == nil {
		t.Error("findEXIFBlock invented an EXIF block")
	}
	// The marker appearing in image data must not be mistaken for a segment.
	body := append(makeJPEG(t, 20, 20), []byte("Exif\x00\x00II*\x00")...)
	if _, err := findEXIFBlock(body); err == nil {
		t.Error("findEXIFBlock matched the marker inside image data")
	}
}

func TestParseEXIFReadsOrientation(t *testing.T) {
	// A little-endian TIFF header with one IFD entry: orientation = 6.
	b := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	b = append(b, 1, 0) // one entry
	b = append(b, 0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0)
	b = append(b, 0, 0, 0, 0) // no next IFD

	got, err := parseEXIF(b)
	if err != nil {
		t.Fatalf("parseEXIF: %v", err)
	}
	if got.Orientation != 6 {
		t.Errorf("orientation = %d, want 6", got.Orientation)
	}
}

// TestParseEXIFSurvivesRubbish: the offsets come out of a file somebody else
// wrote, and a forged one must not loop or read past the end.
func TestParseEXIFSurvivesRubbish(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":           {},
		"short":           {'I', 'I'},
		"bad magic":       {'X', 'X', 42, 0, 8, 0, 0, 0},
		"bad check":       {'I', 'I', 99, 0, 8, 0, 0, 0},
		"offset past end": {'I', 'I', 42, 0, 0xff, 0xff, 0xff, 0xff},
		"self-referencing": append([]byte{'I', 'I', 42, 0, 8, 0, 0, 0, 0, 0},
			[]byte{8, 0, 0, 0}...),
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = parseEXIF(b)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("parseEXIF(%s) did not finish", name)
		}
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 2)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	key := Key(1, 42, "etag-abc", 256)
	if got := c.Get(key); got != nil {
		t.Error("an empty cache returned something")
	}
	c.Put(key, []byte("preview-bytes"))
	if got := c.Get(key); string(got) != "preview-bytes" {
		t.Errorf("Get = %q, want the stored bytes", got)
	}

	// The ETag is part of the key, so an edited file misses rather than being
	// served the old picture.
	if got := c.Get(Key(1, 42, "etag-xyz", 256)); got != nil {
		t.Error("a changed ETag hit the cache")
	}
	// And so is the account.
	if got := c.Get(Key(2, 42, "etag-abc", 256)); got != nil {
		t.Error("another account's key hit the cache")
	}
}

func TestCachePrunesByAge(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 1)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	fresh, stale := Key(1, 1, "a", 64), Key(1, 2, "b", 64)
	c.Put(fresh, []byte("fresh"))
	c.Put(stale, []byte("stale"))

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(c.path(stale), old, old); err != nil {
		t.Fatalf("age the entry: %v", err)
	}

	removed, freed, err := c.Prune(24 * time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 || freed != int64(len("stale")) {
		t.Errorf("Prune removed %d entries freeing %d bytes, want 1 and %d",
			removed, freed, len("stale"))
	}
	if c.Get(fresh) == nil {
		t.Error("Prune removed an entry that was still in use")
	}
	if c.Get(stale) != nil {
		t.Error("Prune left the stale entry")
	}
}

func TestCacheConcurrencyIsBounded(t *testing.T) {
	c, err := NewCache(filepath.Join(t.TempDir(), "p"), 2)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	never := make(chan struct{})
	r1, ok1 := c.Acquire(never)
	r2, ok2 := c.Acquire(never)
	if !ok1 || !ok2 {
		t.Fatal("could not take the two available slots")
	}

	// A third must wait; giving up has to be possible, or a client that hangs
	// up would hold a request open forever.
	gone := make(chan struct{})
	close(gone)
	if _, ok := c.Acquire(gone); ok {
		t.Error("Acquire handed out a third slot when only two exist")
	}
	r1()
	r2()

	// Released, so it is available again.
	if _, ok := c.Acquire(never); !ok {
		t.Error("a released slot was not reusable")
	}
}
