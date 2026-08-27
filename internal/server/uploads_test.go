package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// upload drives the chunked upload protocol: create the transfer, PUT each
// chunk, then MOVE the assemble marker to the destination.
func (h *harness) upload(t *testing.T, transfer, dest string, chunks map[string]string, moveHeaders map[string]string) *http.Response {
	t.Helper()
	base := "/remote.php/dav/uploads/alice/" + transfer

	resp := h.do("MKCOL", base, "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL %s: status = %d, want 201", base, resp.StatusCode)
	}
	resp.Body.Close()

	for name, body := range chunks {
		resp := h.do(http.MethodPut, base+"/"+name, "alice", alicePassword, body, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT chunk %s: status = %d, want 201", name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	headers := map[string]string{"Destination": h.http.URL + dest}
	for k, v := range moveHeaders {
		headers[k] = v
	}
	return h.do("MOVE", base+"/.file", "alice", alicePassword, "", headers)
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	h := newHarness(t)
	resp := h.upload(t, "transfer-1", "/remote.php/dav/files/alice/big.txt",
		map[string]string{"1": "part one ", "2": "part two ", "3": "part three"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("assemble: status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("OC-FileId") == "" {
		t.Error("no OC-FileId after assembly")
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("no ETag after assembly")
	}
	resp.Body.Close()

	want := "part one part two part three"
	got, err := os.ReadFile(filepath.Join(h.homes["alice"], "big.txt"))
	if err != nil || string(got) != want {
		t.Fatalf("assembled file = %q, %v; want %q", got, err, want)
	}

	resp = h.do(http.MethodGet, "/remote.php/dav/files/alice/big.txt", "alice", alicePassword, "", nil)
	if body := readBody(t, resp); body != want {
		t.Errorf("download after assembly = %q, want %q", body, want)
	}
}

// TestChunkedUploadOrdersNumerically is the trap this protocol sets. Chunks are
// joined in the order of their names, and a lexicographic sort puts "10" before
// "9" - silently producing a corrupt file rather than an error.
func TestChunkedUploadOrdersNumerically(t *testing.T) {
	h := newHarness(t)

	chunks := make(map[string]string, 12)
	var want strings.Builder
	for i := 1; i <= 12; i++ {
		piece := "[" + strconv.Itoa(i) + "]"
		chunks[strconv.Itoa(i)] = piece
		want.WriteString(piece)
	}

	resp := h.upload(t, "ordered", "/remote.php/dav/files/alice/ordered.txt", chunks, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("assemble: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	got, err := os.ReadFile(filepath.Join(h.homes["alice"], "ordered.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != want.String() {
		t.Errorf("chunks were joined out of order:\n got %s\nwant %s", got, want.String())
	}
}

// TestChunkedUploadAcceptsPaddedNames: clients pad chunk numbers
// inconsistently, and padded and unpadded names must sort the same way.
func TestChunkedUploadAcceptsPaddedNames(t *testing.T) {
	h := newHarness(t)
	resp := h.upload(t, "padded", "/remote.php/dav/files/alice/padded.txt",
		map[string]string{"00001": "a", "00002": "b", "00010": "j"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("assemble: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	got, _ := os.ReadFile(filepath.Join(h.homes["alice"], "padded.txt"))
	if string(got) != "abj" {
		t.Errorf("assembled = %q, want %q", got, "abj")
	}
}

// TestChunkedUploadResume covers what chunking exists for: after an
// interruption the client asks what arrived and sends only the rest.
func TestChunkedUploadResume(t *testing.T) {
	h := newHarness(t)
	base := "/remote.php/dav/uploads/alice/resumable"

	resp := h.do("MKCOL", base, "alice", alicePassword, "", nil)
	resp.Body.Close()
	for _, name := range []string{"1", "2"} {
		resp := h.do(http.MethodPut, base+"/"+name, "alice", alicePassword, "chunk"+name+" ", nil)
		resp.Body.Close()
	}

	// The client asks which chunks the server holds.
	resp = h.propfind(base, "alice", alicePassword, "1")
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND on a transfer: status = %d, want 207", resp.StatusCode)
	}
	doc := parseMultistatus(t, readBody(t, resp))
	hrefs := strings.Join(doc.hrefs(), " ")
	for _, want := range []string{base + "/1", base + "/2"} {
		if !strings.Contains(hrefs, want) {
			t.Errorf("listing is missing %q; got %v", want, doc.hrefs())
		}
	}
	if strings.Contains(hrefs, base+"/3") {
		t.Error("listing reported a chunk that was never uploaded")
	}

	// It then sends only the missing chunk and assembles.
	resp = h.do(http.MethodPut, base+"/3", "alice", alicePassword, "chunk3", nil)
	resp.Body.Close()
	resp = h.do("MOVE", base+"/.file", "alice", alicePassword, "", map[string]string{
		"Destination": h.http.URL + "/remote.php/dav/files/alice/resumed.txt",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("assemble: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	got, _ := os.ReadFile(filepath.Join(h.homes["alice"], "resumed.txt"))
	if string(got) != "chunk1 chunk2 chunk3" {
		t.Errorf("resumed upload = %q", got)
	}
}

// TestChunkedUploadRejectsIncompleteTransfer: joining chunks when some are
// missing would publish a silently truncated file, which is worse than failing.
func TestChunkedUploadRejectsIncompleteTransfer(t *testing.T) {
	h := newHarness(t)
	resp := h.upload(t, "incomplete", "/remote.php/dav/files/alice/truncated.txt",
		map[string]string{"1": "only this"},
		map[string]string{"OC-Total-Length": "9999"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	if _, err := os.Stat(filepath.Join(h.homes["alice"], "truncated.txt")); !os.IsNotExist(err) {
		t.Error("an incomplete transfer was published anyway")
	}
	// The chunks survive, so the client can send the rest rather than restart.
	resp = h.propfind("/remote.php/dav/uploads/alice/incomplete", "alice", alicePassword, "1")
	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("transfer was discarded after a failed assembly: status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestChunkedUploadVerifiesChecksum(t *testing.T) {
	h := newHarness(t)
	sum := sha1.Sum([]byte("abc"))

	// The checksum covers the reassembled file, not any single chunk.
	resp := h.upload(t, "summed", "/remote.php/dav/files/alice/summed.txt",
		map[string]string{"1": "a", "2": "b", "3": "c"},
		map[string]string{"OC-Checksum": "SHA1:" + hex.EncodeToString(sum[:])})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("matching checksum: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.upload(t, "badsum", "/remote.php/dav/files/alice/badsum.txt",
		map[string]string{"1": "x", "2": "y"},
		map[string]string{"OC-Checksum": "SHA1:" + strings.Repeat("0", 40)})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched checksum: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(h.homes["alice"], "badsum.txt")); !os.IsNotExist(err) {
		t.Error("a file failing its checksum was published")
	}
}

func TestChunkedUploadDiscard(t *testing.T) {
	h := newHarness(t)
	base := "/remote.php/dav/uploads/alice/abandoned"

	resp := h.do("MKCOL", base, "alice", alicePassword, "", nil)
	resp.Body.Close()
	resp = h.do(http.MethodPut, base+"/1", "alice", alicePassword, "data", nil)
	resp.Body.Close()

	resp = h.do(http.MethodDelete, base, "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE transfer: status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.propfind(base, "alice", alicePassword, "1")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("discarded transfer still present: status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestUploadScratchIsInvisible: chunks live inside the user's home, so they
// must not surface as files - to a sync client or to the index.
func TestUploadScratchIsInvisible(t *testing.T) {
	h := newHarness(t)
	base := "/remote.php/dav/uploads/alice/hidden"
	resp := h.do("MKCOL", base, "alice", alicePassword, "", nil)
	resp.Body.Close()
	resp = h.do(http.MethodPut, base+"/1", "alice", alicePassword, "in flight", nil)
	resp.Body.Close()

	// It really is on disk inside the home directory...
	if _, err := os.Stat(filepath.Join(h.homes["alice"], ".mirage-uploads", "hidden", "1")); err != nil {
		t.Fatalf("chunk not written where expected: %v", err)
	}
	// ...but invisible to the client.
	resp = h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "infinity")
	if body := readBody(t, resp); strings.Contains(body, "mirage-uploads") {
		t.Error("the upload scratch area appeared in a file listing")
	}

	if err := h.server.scanner.ScanAll(context.Background(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	resp = h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "infinity")
	if body := readBody(t, resp); strings.Contains(body, "mirage-uploads") {
		t.Error("a rescan indexed the upload scratch area")
	}
}

// TestChunkedUploadCannotEscapeTheAccount applies the isolation guarantee to
// the upload endpoint, which takes both a path and a Destination header.
func TestChunkedUploadCannotEscapeTheAccount(t *testing.T) {
	h := newHarness(t)

	t.Run("transfer in another account", func(t *testing.T) {
		resp := h.do("MKCOL", "/remote.php/dav/uploads/bob/intruder", "alice", alicePassword, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("assembling into another account", func(t *testing.T) {
		base := "/remote.php/dav/uploads/alice/sneaky"
		resp := h.do("MKCOL", base, "alice", alicePassword, "", nil)
		resp.Body.Close()
		resp = h.do(http.MethodPut, base+"/1", "alice", alicePassword, "payload", nil)
		resp.Body.Close()

		resp = h.do("MOVE", base+"/.file", "alice", alicePassword, "", map[string]string{
			"Destination": h.http.URL + "/remote.php/dav/files/bob/planted.txt",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		resp.Body.Close()
		if _, err := os.Stat(filepath.Join(h.homes["bob"], "planted.txt")); !os.IsNotExist(err) {
			t.Fatal("an assembly wrote into bob's home directory")
		}
	})

	t.Run("transfer id escaping its directory", func(t *testing.T) {
		for _, id := range []string{"..", "..%2f..%2fescape", "a%2fb"} {
			resp := h.do("MKCOL", "/remote.php/dav/uploads/alice/"+id, "alice", alicePassword, "", nil)
			if resp.StatusCode == http.StatusCreated {
				t.Errorf("transfer id %q was accepted", id)
			}
			resp.Body.Close()
		}
	})

	t.Run("non-numeric chunk name", func(t *testing.T) {
		base := "/remote.php/dav/uploads/alice/named"
		resp := h.do("MKCOL", base, "alice", alicePassword, "", nil)
		resp.Body.Close()
		// Chunks are joined in numeric order, so a name with no numeric value
		// has no defined position.
		resp = h.do(http.MethodPut, base+"/final", "alice", alicePassword, "x", nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		resp.Body.Close()
	})
}

func TestChunkingIsAdvertised(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", "", "", "", nil)
	body := readBody(t, resp)
	if !strings.Contains(body, `"chunking":"1.0"`) {
		t.Errorf("capabilities do not advertise chunking; got:\n%s", body)
	}
	if !strings.Contains(body, `"bigfilechunking":true`) {
		t.Error("capabilities do not advertise bigfilechunking")
	}
}
