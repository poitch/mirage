package preview

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// TestExtractHEIFThumbnail runs against a real photograph from a phone.
//
// Opt-in, because a repository cannot carry somebody's photograph. This is the
// test that matters: the format has enough variation between cameras that a
// synthetic file proves only that the parser reads what this test wrote.
func TestExtractHEIFThumbnail(t *testing.T) {
	path := os.Getenv("MIRAGE_SAMPLE_HEIC")
	if path == "" {
		t.Skip("set MIRAGE_SAMPLE_HEIC to a photograph from a phone to run this")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	info, _ := f.Stat()

	got, err := extractHEIFThumbnail(f)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	t.Logf("%.1f MB source -> %d byte thumbnail, %dx%d",
		float64(info.Size())/(1<<20), len(got.Data), got.Width, got.Height)

	// The point of the exercise: what is served must be a tiny fraction of what
	// was read, or decoding the original would have been just as good.
	if int64(len(got.Data))*20 > info.Size() {
		t.Errorf("the thumbnail is %d bytes of a %d byte file, which is not a saving worth having",
			len(got.Data), info.Size())
	}
	if got.Width < 128 || got.Height < 128 {
		t.Errorf("thumbnail is %dx%d, too small to serve a listing", got.Width, got.Height)
	}
	// A valid HEIF file begins with a file-type box naming a HEIF brand.
	if len(got.Data) < 16 || string(got.Data[4:8]) != "ftyp" {
		t.Fatalf("the result does not begin with a file-type box")
	}
	if !bytes.Contains(got.Data[:32], []byte("heic")) {
		t.Errorf("the file-type box does not claim a HEIC brand")
	}
	// Its metadata must describe exactly one picture.
	if !bytes.Contains(got.Data, []byte("hvc1")) {
		t.Error("the result declares no coded picture")
	}
	if !bytes.Contains(got.Data, []byte("hvcC")) {
		t.Error("the result carries no decoder configuration, so nothing can read it")
	}
	if !bytes.Contains(got.Data, []byte("mdat")) {
		t.Error("the result carries no picture data")
	}
}

// TestExtractHEIFRefusesRubbish: the sizes and offsets all come out of a file
// somebody else wrote, and a forged one must not read past the end or allocate
// on demand.
func TestExtractHEIFRefusesRubbish(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"truncated header": {0, 0, 0},
		"not a HEIF":       []byte("this is a text file, honestly"),
		"a box claiming to be enormous": append(
			be32(0xFFFFFFF0), append([]byte("meta"), make([]byte, 32)...)...),
		"a box smaller than its header": append(be32(4), []byte("meta")...),
		"meta with nothing in it": append(
			be32(16), append([]byte("meta"), make([]byte, 8)...)...),
	}
	for name, data := range cases {
		if _, err := extractHEIFThumbnail(bytes.NewReader(data)); err == nil {
			t.Errorf("extractHEIFThumbnail accepted %s", name)
		}
	}
}

// TestBuildHEIFIsSelfConsistent: the container records where the picture starts,
// and that offset depends on the size of the box recording it. Getting that
// wrong produces a file that looks right and decodes to nothing.
func TestBuildHEIFIsSelfConsistent(t *testing.T) {
	ispe := makeBox("ispe", append(make([]byte, 4), append(be32(320), be32(240)...)...))
	hvcC := makeBox("hvcC", []byte{0x01, 0x02, 0x03, 0x04})
	payload := bytes.Repeat([]byte{0xAB}, 1234)

	out, err := buildHEIF(itemProps{
		Boxes:     [][]byte{ispe, hvcC},
		Essential: []bool{false, true},
		Width:     320, Height: 240,
	}, payload)
	if err != nil {
		t.Fatalf("buildHEIF: %v", err)
	}

	// Find the mdat box and check the recorded offset lands on the payload.
	idx := bytes.Index(out, []byte("mdat"))
	if idx < 4 {
		t.Fatal("no mdat box")
	}
	dataStart := idx + 4
	if !bytes.Equal(out[dataStart:dataStart+len(payload)], payload) {
		t.Error("the payload is not where the mdat box says it is")
	}

	// The offset recorded in iloc must equal that position.
	iloc := bytes.Index(out, []byte("iloc"))
	if iloc < 0 {
		t.Fatal("no iloc box")
	}
	// iloc payload: version+flags(4), sizes(2), count(2), id(2), dref(2),
	// extent count(2), then the offset.
	at := iloc + 4 + 4 + 2 + 2 + 2 + 2 + 2
	recorded := binary.BigEndian.Uint32(out[at : at+4])
	if int(recorded) != dataStart {
		t.Errorf("iloc records the picture at %d, but it is at %d", recorded, dataStart)
	}
}
