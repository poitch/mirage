package server

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePhoto puts a real JPEG in an account's files.
func writePhoto(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x30, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// harnessWithPhoto adds a picture and reindexes.
func harnessWithPhoto(t *testing.T) (*harness, int64) {
	t.Helper()
	h := newHarness(t)
	writePhoto(t, filepath.Join(h.homes["alice"], "holiday.jpg"), 800, 600)
	writePhoto(t, filepath.Join(h.homes["bob"], "private.jpg"), 400, 300)
	if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	id, err := nodeIDFor(t, h, aliceID(t, h), "holiday.jpg")
	if err != nil {
		t.Fatalf("find the photo: %v", err)
	}
	return h, id
}

func TestPreviewIsServed(t *testing.T) {
	h, id := harnessWithPhoto(t)
	resp := h.do("GET", previewURL(id, 256), "alice", alicePassword, "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	img, err := jpeg.Decode(bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("the preview is not a JPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() > 256 || b.Dy() > 256 {
		t.Errorf("preview is %dx%d, which is over the requested box", b.Dx(), b.Dy())
	}
	// The shape is preserved: an 800x600 photo fits as 256x192.
	if b.Dx() != 256 || b.Dy() != 192 {
		t.Errorf("preview is %dx%d, want 256x192", b.Dx(), b.Dy())
	}
}

// TestPreviewIsCached: the second request must not read the photograph again.
// On a share this size that is the difference between a gallery loading and the
// array thrashing.
func TestPreviewIsCached(t *testing.T) {
	h, id := harnessWithPhoto(t)

	first := h.do("GET", previewURL(id, 256), "alice", alicePassword, "", nil)
	firstBody := readBody(t, first)
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a preview")
	}

	second := h.do("GET", previewURL(id, 256), "alice", alicePassword, "", nil)
	secondBody := readBody(t, second)
	if firstBody != secondBody {
		t.Error("two requests for the same preview produced different bytes")
	}

	conditional := h.do("GET", previewURL(id, 256), "alice", alicePassword, "",
		map[string]string{"If-None-Match": etag})
	body := readBody(t, conditional)
	if conditional.StatusCode != http.StatusNotModified {
		t.Errorf("conditional request = %d, want 304", conditional.StatusCode)
	}
	if body != "" {
		t.Errorf("a 304 carried %d bytes", len(body))
	}
}

// TestPreviewCannotReachAnotherAccount: the file id is the only input, so
// nothing but the lookup confines this.
func TestPreviewCannotReachAnotherAccount(t *testing.T) {
	h, _ := harnessWithPhoto(t)
	bob, err := h.db.UserByName(t.Context(), "bob")
	if err != nil {
		t.Fatalf("look up bob: %v", err)
	}
	bobPhoto, err := nodeIDFor(t, h, bob.ID, "private.jpg")
	if err != nil {
		t.Fatalf("find bob's photo: %v", err)
	}

	resp := h.do("GET", previewURL(bobPhoto, 256), "alice", alicePassword, "", nil)
	readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for another account's photo", resp.StatusCode)
	}

	// And by path, which is the other way clients address it.
	byPath := h.do("GET", "/index.php/core/preview?file=../bob/private.jpg&x=256&y=256",
		"alice", alicePassword, "", nil)
	readBody(t, byPath)
	if byPath.StatusCode != http.StatusNotFound {
		t.Errorf("by-path status = %d, want 404", byPath.StatusCode)
	}
}

// TestPreviewOfAnUnpreviewableFileIs404: most files are not pictures, and the
// client draws its own icon rather than being given a placeholder.
func TestPreviewOfAnUnpreviewableFileIs404(t *testing.T) {
	h, _ := harnessWithPhoto(t)
	id, err := nodeIDFor(t, h, aliceID(t, h), "hello.txt")
	if err != nil {
		t.Fatalf("find the text file: %v", err)
	}
	resp := h.do("GET", previewURL(id, 256), "alice", alicePassword, "", nil)
	readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a text file", resp.StatusCode)
	}
}

func TestPreviewOfADirectoryIs404(t *testing.T) {
	h, _ := harnessWithPhoto(t)
	id, err := nodeIDFor(t, h, aliceID(t, h), "docs")
	if err != nil {
		t.Fatalf("find the folder: %v", err)
	}
	resp := h.do("GET", previewURL(id, 256), "alice", alicePassword, "", nil)
	readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a folder", resp.StatusCode)
	}
}

func TestPreviewRequiresAuth(t *testing.T) {
	h, id := harnessWithPhoto(t)
	resp := h.do("GET", previewURL(id, 256), "", "", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestPreviewSizesAreBucketed: clients ask for whatever their tile happens to
// be, and honouring each exactly would mean a copy of every photo per layout.
func TestPreviewSizesAreBucketed(t *testing.T) {
	h, id := harnessWithPhoto(t)
	etagFor := func(size int) string {
		resp := h.do("GET", previewURL(id, size), "alice", alicePassword, "", nil)
		resp.Body.Close()
		return resp.Header.Get("ETag")
	}
	// 200 and 250 both round up to 256, so they are the same preview.
	if etagFor(200) != etagFor(250) {
		t.Error("two sizes in the same bucket produced different previews")
	}
	if etagFor(200) == etagFor(600) {
		t.Error("sizes in different buckets shared a preview")
	}
}

// TestPreviewCapabilityIsAdvertised so a client does not offer a gallery whose
// tiles would all come back empty.
func TestPreviewCapabilityIsAdvertised(t *testing.T) {
	h, _ := harnessWithPhoto(t)
	body := readBody(t, h.do("GET", "/ocs/v2.php/cloud/capabilities?format=json",
		"alice", alicePassword, "", nil))
	for _, want := range []string{"files_previews", "image/jpeg"} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Errorf("capabilities do not mention %q", want)
		}
	}
}

func previewURL(fileID int64, size int) string {
	return fmt.Sprintf("/index.php/core/preview?fileId=%d&x=%d&y=%d", fileID, size, size)
}

// TestListingsAdvertiseAPreview is what actually makes previews reach a phone.
// Clients read nc:has-preview and only fetch a thumbnail when it says yes, so
// a listing that reports false means the preview endpoint is never called and
// the feature looks broken however well it works.
func TestListingsAdvertiseAPreview(t *testing.T) {
	h, _ := harnessWithPhoto(t)

	// The photo has to appear in a listing at all before anything reads its
	// properties.
	body := readBody(t, h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "1"))
	if !strings.Contains(body, "holiday.jpg") {
		t.Fatalf("the photo is not in the listing:\n%s", body)
	}

	if got := propValue(t, h, "holiday.jpg", "http://nextcloud.org/ns", "has-preview"); got != "true" {
		t.Errorf("has-preview for a photo = %q, want true; clients will not ask for a thumbnail", got)
	}
	// And not claimed for things that have none, or a client fetches a picture
	// only to be told there isn't one.
	if got := propValue(t, h, "hello.txt", "http://nextcloud.org/ns", "has-preview"); got != "false" {
		t.Errorf("has-preview for a text file = %q, want false", got)
	}
	if got := propValue(t, h, "docs", "http://nextcloud.org/ns", "has-preview"); got != "false" {
		t.Errorf("has-preview for a folder = %q, want false", got)
	}
}

func propValue(t *testing.T, h *harness, path, space, local string) string {
	t.Helper()
	return h.prop(t, "/remote.php/dav/files/alice/"+path, space, local)
}
