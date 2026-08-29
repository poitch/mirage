package preview

import (
	"bytes"
	"image/jpeg"
	"os"
	"testing"
	"time"
)

// TestAgainstARealPhoto runs the generator over an actual camera file.
//
// Opt-in, because a repository cannot carry somebody's photograph: point
// MIRAGE_SAMPLE_PHOTO at one to exercise the paths that synthetic images cannot
// reach, which are the two that matter most - the camera's embedded thumbnail,
// and the rotation that keeps a phone photo the right way up.
func TestAgainstARealPhoto(t *testing.T) {
	path := os.Getenv("MIRAGE_SAMPLE_PHOTO")
	if path == "" {
		t.Skip("set MIRAGE_SAMPLE_PHOTO to a camera JPEG to run this")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open sample: %v", err)
	}
	defer f.Close()

	info, _ := f.Stat()
	exif, block, err := readEXIF(f, path)
	if err != nil {
		t.Fatalf("no EXIF found in %s: %v", path, err)
	}
	t.Logf("file %.1f MB, orientation %d, thumbnail %d bytes at offset %d",
		float64(info.Size())/(1<<20), exif.Orientation, exif.ThumbLength, exif.ThumbOffset)
	if thumb := exif.thumbnail(block); thumb != nil {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(thumb))
		if err != nil {
			t.Errorf("the embedded thumbnail did not decode: %v", err)
		} else {
			t.Logf("embedded thumbnail is %dx%d", cfg.Width, cfg.Height)
		}
	}

	for _, size := range []int{64, 128, 256, 512} {
		start := time.Now()
		got, err := Generate(f, path, size)
		if err != nil {
			t.Fatalf("Generate(%d): %v", size, err)
		}
		took := time.Since(start)
		if got.Width > size || got.Height > size {
			t.Errorf("size %d produced %dx%d, which is over the bound",
				size, got.Width, got.Height)
		}
		if len(got.Data) == 0 {
			t.Errorf("size %d produced no data", size)
		}
		source := "decoded the full image"
		if got.FromThumbnail {
			source = "used the embedded thumbnail"
		}
		t.Logf("size %-4d -> %dx%d, %5.1f kB, %8s, %s",
			size, got.Width, got.Height, float64(len(got.Data))/1024, took.Round(time.Millisecond), source)
	}
}
