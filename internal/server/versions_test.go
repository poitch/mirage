package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// saveFile writes new contents through the client, as an edit would.
func (h *harness) saveFile(t *testing.T, path, body string) {
	t.Helper()
	resp := h.do("PUT", "/remote.php/dav/files/alice/"+path, "alice", alicePassword, body, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT %s: status = %d", path, resp.StatusCode)
	}
}

func (h *harness) versionList(t *testing.T, fileID int64) multistatusDoc {
	t.Helper()
	resp := h.do("PROPFIND", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d", fileID),
		"alice", alicePassword, "", map[string]string{"Depth": "1"})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("version listing: status = %d, want 207: %s", resp.StatusCode, body)
	}
	return parseMultistatus(t, body)
}

// versionStamps pulls the timestamps out of a listing.
func versionStamps(doc multistatusDoc) []string {
	var out []string
	for _, r := range doc.Responses {
		if strings.HasSuffix(r.Href, "/") {
			continue // the collection itself
		}
		out = append(out, r.Href[strings.LastIndex(r.Href, "/")+1:])
	}
	return out
}

func TestOverwritingKeepsTheEarlierContents(t *testing.T) {
	h := newHarness(t)
	id := h.fileIDInt(t, "hello.txt")

	if stamps := versionStamps(h.versionList(t, id)); len(stamps) != 0 {
		t.Fatalf("a file that has never been overwritten has versions: %v", stamps)
	}

	h.saveFile(t, "hello.txt", "second draft")
	stamps := versionStamps(h.versionList(t, id))
	if len(stamps) != 1 {
		t.Fatalf("after one overwrite there are %d versions, want 1", len(stamps))
	}

	// The version holds what was there before, not what is there now.
	got := h.do("GET", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d/%s", id, stamps[0]),
		"alice", alicePassword, "", nil)
	body := readBody(t, got)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("fetching a version: status = %d", got.StatusCode)
	}
	if body != "hello from alice" {
		t.Errorf("version contents = %q, want the original", body)
	}
	// And the live file is the new one.
	live := readBody(t, h.do("GET", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, "", nil))
	if live != "second draft" {
		t.Errorf("live contents = %q, want the new draft", live)
	}
}

// TestVersionsAreNotSyncedBack: like the trash, versions live inside the
// account's own directory, so an indexed versions folder would push a copy of
// every earlier draft to every device.
func TestVersionsAreNotSyncedBack(t *testing.T) {
	h := newHarness(t)
	h.saveFile(t, "hello.txt", "second draft")

	entries, err := os.ReadDir(filepath.Join(h.homes["alice"], fsx.VersionsDir))
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected a versions directory on disk, got %v (%v)", entries, err)
	}

	for range 2 {
		if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
			t.Fatalf("rescan: %v", err)
		}
	}
	if _, err := store.NodeByPath(t.Context(), h.db, aliceID(t, h), fsx.VersionsDir); err == nil {
		t.Error("the versions directory was indexed; every draft would sync to every client")
	}
	listing := readBody(t, h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "infinity"))
	if strings.Contains(listing, fsx.VersionsDir) {
		t.Errorf("the versions directory appears in a file listing:\n%s", listing)
	}
}

func TestRestoringAVersion(t *testing.T) {
	h := newHarness(t)
	id := h.fileIDInt(t, "hello.txt")
	h.saveFile(t, "hello.txt", "second draft")

	stamps := versionStamps(h.versionList(t, id))
	resp := h.do("MOVE", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d/%s", id, stamps[0]),
		"alice", alicePassword, "",
		map[string]string{"Destination": "/remote.php/dav/versions/alice/restore/target"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore: status = %d, want 204", resp.StatusCode)
	}

	live := readBody(t, h.do("GET", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, "", nil))
	if live != "hello from alice" {
		t.Errorf("after restoring, contents = %q, want the original", live)
	}
}

// TestRestoringIsItselfUndoable: somebody who restores the wrong version has
// not lost the contents they had a moment ago.
func TestRestoringIsItselfUndoable(t *testing.T) {
	h := newHarness(t)
	id := h.fileIDInt(t, "hello.txt")
	h.saveFile(t, "hello.txt", "second draft")

	stamps := versionStamps(h.versionList(t, id))
	resp := h.do("MOVE", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d/%s", id, stamps[0]),
		"alice", alicePassword, "",
		map[string]string{"Destination": "/remote.php/dav/versions/alice/restore/target"})
	resp.Body.Close()

	after := versionStamps(h.versionList(t, id))
	if len(after) < 1 {
		t.Fatal("restoring left no way back")
	}
	// One of the versions now holds the draft that was live before the restore.
	var found bool
	for _, s := range after {
		body := readBody(t, h.do("GET",
			fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d/%s", id, s),
			"alice", alicePassword, "", nil))
		if body == "second draft" {
			found = true
		}
	}
	if !found {
		t.Errorf("the contents replaced by the restore were not kept: versions %v", after)
	}
}

// TestVersionCountIsCapped: a file saved repeatedly must not accumulate
// hundreds of copies of itself.
func TestVersionCountIsCapped(t *testing.T) {
	h := newHarnessWith(t, func(cfg *harnessConfig) { cfg.maxVersions = 3 })
	id := h.fileIDInt(t, "hello.txt")

	for i := range 6 {
		h.saveFile(t, "hello.txt", fmt.Sprintf("draft %d", i))
	}
	stamps := versionStamps(h.versionList(t, id))
	if len(stamps) > 3 {
		t.Errorf("kept %d versions, want at most 3", len(stamps))
	}
	if len(stamps) == 0 {
		t.Error("kept no versions at all")
	}
}

// TestLargeFilesAreNotVersioned: keeping a version means copying the file, and
// without a bound one large video saved twice costs more than everything else.
func TestLargeFilesAreNotVersioned(t *testing.T) {
	h := newHarnessWith(t, func(cfg *harnessConfig) { cfg.maxVersionBytes = 8 })
	id := h.fileIDInt(t, "hello.txt")

	h.saveFile(t, "hello.txt", "a longer body than the limit")
	if stamps := versionStamps(h.versionList(t, id)); len(stamps) != 0 {
		t.Errorf("a file over the size limit was versioned: %v", stamps)
	}
	// The save itself still worked.
	body := readBody(t, h.do("GET", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, "", nil))
	if body != "a longer body than the limit" {
		t.Errorf("the save was affected by not versioning: got %q", body)
	}
}

func TestDeletingAVersion(t *testing.T) {
	h := newHarness(t)
	id := h.fileIDInt(t, "hello.txt")
	h.saveFile(t, "hello.txt", "second draft")

	stamps := versionStamps(h.versionList(t, id))
	resp := h.do("DELETE", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d/%s", id, stamps[0]),
		"alice", alicePassword, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete version: status = %d, want 204", resp.StatusCode)
	}
	if left := versionStamps(h.versionList(t, id)); len(left) != 0 {
		t.Errorf("the version survived deletion: %v", left)
	}
}

// TestVersionsCannotReachAnotherAccount: a file id is the only input, so
// nothing but the lookup confines this.
func TestVersionsCannotReachAnotherAccount(t *testing.T) {
	h := newHarness(t)
	bobResp := h.do("PUT", "/remote.php/dav/files/bob/secret.txt", "bob", bobPassword,
		"bob's second draft", nil)
	bobResp.Body.Close()

	bob, err := h.db.UserByName(t.Context(), "bob")
	if err != nil {
		t.Fatalf("look up bob: %v", err)
	}
	bobFile, err := nodeIDFor(t, h, bob.ID, "secret.txt")
	if err != nil {
		t.Fatalf("find bob's file: %v", err)
	}

	for _, tc := range []struct{ method, path string }{
		{"PROPFIND", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d", bobFile)},
		{"PROPFIND", fmt.Sprintf("/remote.php/dav/versions/bob/versions/%d", bobFile)},
		{"GET", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d/1", bobFile)},
	} {
		resp := h.do(tc.method, tc.path, "alice", alicePassword, "",
			map[string]string{"Depth": "1"})
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
		if strings.Contains(body, "bob's private data") || strings.Contains(body, "second draft") {
			t.Errorf("%s %s leaked bob's contents", tc.method, tc.path)
		}
	}
}

func TestVersionsRequireAuth(t *testing.T) {
	h := newHarness(t)
	id := h.fileIDInt(t, "hello.txt")
	resp := h.do("PROPFIND", fmt.Sprintf("/remote.php/dav/versions/alice/versions/%d", id),
		"", "", "", map[string]string{"Depth": "1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestVersioningIsAdvertised(t *testing.T) {
	h := newHarness(t)
	body := readBody(t, h.do("GET", "/ocs/v2.php/cloud/capabilities?format=json",
		"alice", alicePassword, "", nil))
	if !strings.Contains(body, `"versioning":true`) {
		t.Errorf("capabilities do not advertise versioning:\n%s", body)
	}
}
