package avatar

import (
	"bytes"
	"image/png"
	"testing"
)

func TestGenerateProducesAPNGOfTheRequestedSize(t *testing.T) {
	for _, size := range []int{16, 64, 128, 512} {
		data, err := Generate("alice", size)
		if err != nil {
			t.Fatalf("Generate(%d): %v", size, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode %d: %v", size, err)
		}
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Errorf("size %d produced a %dx%d image", size, b.Dx(), b.Dy())
		}
	}
}

// TestGenerateIsStable: the image is the account's identity, so it must not
// change between requests or between restarts.
func TestGenerateIsStable(t *testing.T) {
	first, err := Generate("alice", 128)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	second, err := Generate("alice", 128)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the same seed produced two different images")
	}
}

// TestGenerateDistinguishesAccounts is the point of the whole package: two
// accounts must not end up looking the same.
func TestGenerateDistinguishesAccounts(t *testing.T) {
	seen := make(map[string]string)
	for _, seed := range []string{"alice", "bob", "carol", "dave", "erin", "frank"} {
		data, err := Generate(seed, 128)
		if err != nil {
			t.Fatalf("Generate(%q): %v", seed, err)
		}
		key := string(data)
		if other, dup := seen[key]; dup {
			t.Errorf("%q and %q produced identical avatars", seed, other)
		}
		seen[key] = seed
	}
}

func TestGenerateClampsSize(t *testing.T) {
	for _, tc := range []struct{ ask, want int }{
		{0, MinSize}, {-5, MinSize}, {1, MinSize}, {100000, MaxSize},
	} {
		data, err := Generate("alice", tc.ask)
		if err != nil {
			t.Fatalf("Generate(%d): %v", tc.ask, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got := img.Bounds().Dx(); got != tc.want {
			t.Errorf("size %d produced %d, want clamped to %d", tc.ask, got, tc.want)
		}
	}
}

// TestGenerateIsHorizontallySymmetric checks the mirroring that makes the
// pattern read as a mark rather than as noise.
func TestGenerateIsHorizontallySymmetric(t *testing.T) {
	data, err := Generate("alice", 100)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			mirror := b.Max.X - 1 - (x - b.Min.X)
			if img.At(x, y) != img.At(mirror, y) {
				t.Fatalf("pixel (%d,%d) differs from its mirror (%d,%d)", x, y, mirror, y)
			}
		}
	}
}

func TestETagVariesWithSeedAndSize(t *testing.T) {
	a := ETag("alice", 128)
	if a != ETag("alice", 128) {
		t.Error("ETag is not stable for the same input")
	}
	if a == ETag("bob", 128) {
		t.Error("two accounts share an ETag")
	}
	if a == ETag("alice", 512) {
		t.Error("two sizes share an ETag")
	}
	if len(a) < 3 || a[0] != '"' || a[len(a)-1] != '"' {
		t.Errorf("ETag %q is not a quoted entity tag", a)
	}
}
