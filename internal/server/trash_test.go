package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

func (h *harness) trashList(t *testing.T, user, password string) multistatusDoc {
	t.Helper()
	resp := h.do("PROPFIND", "/remote.php/dav/trashbin/"+user+"/trash/", user, password, "",
		map[string]string{"Depth": "1"})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("trash listing: status = %d, want 207: %s", resp.StatusCode, body)
	}
	return parseMultistatus(t, body)
}

// trashEntryName pulls the trash entry name out of a listing href.
func trashEntryName(t *testing.T, doc multistatusDoc, original string) string {
	t.Helper()
	for _, r := range doc.Responses {
		if strings.HasSuffix(r.Href, "/") {
			continue // the collection itself
		}
		name := r.Href[strings.LastIndex(r.Href, "/")+1:]
		if strings.HasPrefix(name, original+".d") {
			return name
		}
	}
	t.Fatalf("no trash entry for %q in %v", original, doc.hrefs())
	return ""
}

func TestDeleteMovesToTrash(t *testing.T) {
	h := newHarness(t)
	resp := h.do("DELETE", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204", resp.StatusCode)
	}

	// Gone from where it was.
	if _, err := os.Stat(filepath.Join(h.homes["alice"], "hello.txt")); err == nil {
		t.Error("the file is still in place after being deleted")
	}
	// And listed in the trash, with where it came from.
	doc := h.trashList(t, "alice", alicePassword)
	body := readBody(t, h.do("PROPFIND", "/remote.php/dav/trashbin/alice/trash/",
		"alice", alicePassword, "", map[string]string{"Depth": "1"}))
	if !strings.Contains(body, "hello.txt") {
		t.Errorf("the trash listing does not name the file:\n%s", body)
	}
	if !strings.Contains(body, "trashbin-original-location") {
		t.Errorf("the listing does not say where the file came from:\n%s", body)
	}
	if len(doc.Responses) != 2 { // the collection plus the one entry
		t.Errorf("trash holds %d responses, want the collection and one entry", len(doc.Responses))
	}
}

// TestTrashIsNotSyncedBack is the assertion the whole design rests on. The
// trash lives inside the account's own directory, so if it were ever indexed
// every deleted file would come back to every device as a new file.
func TestTrashIsNotSyncedBack(t *testing.T) {
	h := newHarness(t)
	resp := h.do("DELETE", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "", nil)
	resp.Body.Close()

	// It is physically there.
	trashDir := filepath.Join(h.homes["alice"], fsx.TrashDir)
	entries, err := os.ReadDir(trashDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one file in %s, got %v (%v)", trashDir, entries, err)
	}

	// A full rescan must not index it, however many times it runs.
	for range 2 {
		if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
			t.Fatalf("rescan: %v", err)
		}
	}
	if _, err := store.NodeByPath(t.Context(), h.db, aliceID(t, h), fsx.TrashDir); err == nil {
		t.Error("the trash directory was indexed; deleted files would sync back to every client")
	}

	listing := readBody(t, h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "infinity"))
	if strings.Contains(listing, fsx.TrashDir) {
		t.Errorf("the trash directory appears in a file listing:\n%s", listing)
	}
	if strings.Contains(listing, ".d1") {
		t.Errorf("a trashed file appears in a file listing:\n%s", listing)
	}
}

func TestRestoreBringsAFileBack(t *testing.T) {
	h := newHarness(t)
	original := readBody(t, h.do("GET", "/remote.php/dav/files/alice/docs/report.txt",
		"alice", alicePassword, "", nil))

	del := h.do("DELETE", "/remote.php/dav/files/alice/docs/report.txt", "alice", alicePassword, "", nil)
	del.Body.Close()

	name := trashEntryName(t, h.trashList(t, "alice", alicePassword), "report.txt")
	restore := h.do("MOVE", "/remote.php/dav/trashbin/alice/trash/"+name,
		"alice", alicePassword, "",
		map[string]string{"Destination": "/remote.php/dav/trashbin/alice/restore/"})
	restore.Body.Close()
	if restore.StatusCode != http.StatusCreated {
		t.Fatalf("restore: status = %d, want 201", restore.StatusCode)
	}

	// Back where it was, with its contents.
	got := h.do("GET", "/remote.php/dav/files/alice/docs/report.txt", "alice", alicePassword, "", nil)
	body := readBody(t, got)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("the restored file is not readable: status = %d", got.StatusCode)
	}
	if body != original {
		t.Errorf("restored contents = %q, want %q", body, original)
	}
	// And no longer in the trash.
	if doc := h.trashList(t, "alice", alicePassword); len(doc.Responses) != 1 {
		t.Errorf("the entry is still in the trash after being restored: %v", doc.hrefs())
	}
}

// TestRestoreDoesNotOverwrite: restoring must never be a way to lose the file
// that is currently at that path.
func TestRestoreDoesNotOverwrite(t *testing.T) {
	h := newHarness(t)
	del := h.do("DELETE", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "", nil)
	del.Body.Close()

	// Something new takes the name.
	put := h.do("PUT", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword,
		"a different file", nil)
	put.Body.Close()

	name := trashEntryName(t, h.trashList(t, "alice", alicePassword), "hello.txt")
	restore := h.do("MOVE", "/remote.php/dav/trashbin/alice/trash/"+name,
		"alice", alicePassword, "",
		map[string]string{"Destination": "/remote.php/dav/trashbin/alice/restore/"})
	restore.Body.Close()
	if restore.StatusCode != http.StatusCreated {
		t.Fatalf("restore: status = %d, want 201", restore.StatusCode)
	}

	// The newer file is untouched.
	body := readBody(t, h.do("GET", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, "", nil))
	if body != "a different file" {
		t.Errorf("restoring overwrote the file that was there: got %q", body)
	}
	// And the restored one is beside it.
	listing := readBody(t, h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "1"))
	if !strings.Contains(listing, "restored") {
		t.Errorf("the restored file was not placed beside the existing one:\n%s", listing)
	}
}

func TestDeleteAndRestoreADirectory(t *testing.T) {
	h := newHarness(t)
	del := h.do("DELETE", "/remote.php/dav/files/alice/docs", "alice", alicePassword, "", nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status = %d", del.StatusCode)
	}
	// The whole subtree leaves the index.
	for _, p := range []string{"docs", "docs/report.txt", "docs/nested/deep.txt"} {
		if _, err := store.NodeByPath(t.Context(), h.db, aliceID(t, h), p); err == nil {
			t.Errorf("%s is still indexed after its folder was deleted", p)
		}
	}

	name := trashEntryName(t, h.trashList(t, "alice", alicePassword), "docs")
	restore := h.do("MOVE", "/remote.php/dav/trashbin/alice/trash/"+name,
		"alice", alicePassword, "",
		map[string]string{"Destination": "/remote.php/dav/trashbin/alice/restore/"})
	restore.Body.Close()
	if restore.StatusCode != http.StatusCreated {
		t.Fatalf("restore: status = %d, want 201", restore.StatusCode)
	}

	// Restoring a folder brings back everything that was inside it.
	for _, p := range []string{"docs", "docs/report.txt", "docs/nested/deep.txt"} {
		if _, err := store.NodeByPath(t.Context(), h.db, aliceID(t, h), p); err != nil {
			t.Errorf("%s did not come back with its folder: %v", p, err)
		}
	}
}

func TestPermanentDeleteFromTrash(t *testing.T) {
	h := newHarness(t)
	del := h.do("DELETE", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "", nil)
	del.Body.Close()

	name := trashEntryName(t, h.trashList(t, "alice", alicePassword), "hello.txt")
	perm := h.do("DELETE", "/remote.php/dav/trashbin/alice/trash/"+name, "alice", alicePassword, "", nil)
	perm.Body.Close()
	if perm.StatusCode != http.StatusNoContent {
		t.Fatalf("permanent delete: status = %d, want 204", perm.StatusCode)
	}

	if doc := h.trashList(t, "alice", alicePassword); len(doc.Responses) != 1 {
		t.Errorf("the entry survived a permanent delete: %v", doc.hrefs())
	}
	entries, _ := os.ReadDir(filepath.Join(h.homes["alice"], fsx.TrashDir))
	if len(entries) != 0 {
		t.Errorf("the file is still on disk after a permanent delete: %v", entries)
	}
}

func TestEmptyTheTrash(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"hello.txt", "docs/report.txt"} {
		resp := h.do("DELETE", "/remote.php/dav/files/alice/"+p, "alice", alicePassword, "", nil)
		resp.Body.Close()
	}
	if doc := h.trashList(t, "alice", alicePassword); len(doc.Responses) != 3 {
		t.Fatalf("expected two entries in the trash, got %v", doc.hrefs())
	}

	empty := h.do("DELETE", "/remote.php/dav/trashbin/alice/trash", "alice", alicePassword, "", nil)
	empty.Body.Close()
	if empty.StatusCode != http.StatusNoContent {
		t.Fatalf("empty: status = %d, want 204", empty.StatusCode)
	}
	if doc := h.trashList(t, "alice", alicePassword); len(doc.Responses) != 1 {
		t.Errorf("the trash is not empty: %v", doc.hrefs())
	}
}

// TestTrashCannotReachAnotherAccount: the trashbin is addressed by account in
// the path, and an entry name is the only other input.
func TestTrashCannotReachAnotherAccount(t *testing.T) {
	h := newHarness(t)
	del := h.do("DELETE", "/remote.php/dav/files/bob/secret.txt", "bob", bobPassword, "", nil)
	del.Body.Close()

	name := trashEntryName(t, h.trashList(t, "bob", bobPassword), "secret.txt")

	// Alice cannot list, restore from, or empty bob's trash.
	for _, tc := range []struct{ method, path string }{
		{"PROPFIND", "/remote.php/dav/trashbin/bob/trash/"},
		{"DELETE", "/remote.php/dav/trashbin/bob/trash/" + name},
		{"DELETE", "/remote.php/dav/trashbin/bob/trash"},
		{"MOVE", "/remote.php/dav/trashbin/bob/trash/" + name},
	} {
		resp := h.do(tc.method, tc.path, "alice", alicePassword, "",
			map[string]string{"Destination": "/remote.php/dav/trashbin/alice/restore/", "Depth": "1"})
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
		if strings.Contains(body, "secret.txt") {
			t.Errorf("%s %s leaked bob's file", tc.method, tc.path)
		}
	}

	// And bob's entry is still there afterwards.
	if doc := h.trashList(t, "bob", bobPassword); len(doc.Responses) != 2 {
		t.Errorf("bob's trash was disturbed: %v", doc.hrefs())
	}
}

// TestRestoreCannotEscapeTheAccount: the Destination is a URL the client
// supplies, and it names the path the file will be written to.
func TestRestoreCannotEscapeTheAccount(t *testing.T) {
	h := newHarness(t)
	del := h.do("DELETE", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "", nil)
	del.Body.Close()
	name := trashEntryName(t, h.trashList(t, "alice", alicePassword), "hello.txt")

	for _, dest := range []string{
		"/remote.php/dav/trashbin/bob/restore/stolen.txt",
		"/remote.php/dav/trashbin/alice/restore/../../../etc/passwd",
		"/remote.php/dav/files/alice/elsewhere.txt",
	} {
		resp := h.do("MOVE", "/remote.php/dav/trashbin/alice/trash/"+name,
			"alice", alicePassword, "", map[string]string{"Destination": dest})
		readBody(t, resp)
		if resp.StatusCode == http.StatusCreated {
			t.Errorf("Destination %q was accepted", dest)
		}
	}
	if _, err := os.Stat(filepath.Join(h.homes["bob"], "stolen.txt")); err == nil {
		t.Error("a restore wrote into another account")
	}
}

// TestRestoreIgnoresTheDestinationHost matches how MOVE on the files endpoint
// already behaves: Destination is an absolute URI, only its path decides
// anything, and the authority is not trusted to. Recorded as a test because it
// looks like a hole until you notice the path is confined either way.
func TestRestoreIgnoresTheDestinationHost(t *testing.T) {
	h := newHarness(t)
	del := h.do("DELETE", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "", nil)
	del.Body.Close()
	name := trashEntryName(t, h.trashList(t, "alice", alicePassword), "hello.txt")

	resp := h.do("MOVE", "/remote.php/dav/trashbin/alice/trash/"+name, "alice", alicePassword, "",
		map[string]string{
			"Destination": "https://example.com/remote.php/dav/trashbin/alice/restore/moved.txt",
		})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// It landed inside the account, at the path the URL named.
	if _, err := os.Stat(filepath.Join(h.homes["alice"], "moved.txt")); err != nil {
		t.Errorf("the file was not restored to the named path: %v", err)
	}
}

func TestTrashbinRequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do("PROPFIND", "/remote.php/dav/trashbin/alice/trash/", "", "", "",
		map[string]string{"Depth": "1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestUndeleteIsAdvertised, since a client hides its trash view otherwise.
func TestUndeleteIsAdvertised(t *testing.T) {
	h := newHarness(t)
	body := readBody(t, h.do("GET", "/ocs/v2.php/cloud/capabilities?format=json",
		"alice", alicePassword, "", nil))
	if !strings.Contains(body, `"undelete":true`) {
		t.Errorf("capabilities do not advertise undelete:\n%s", body)
	}
}

// TestTrashNamesDoNotCollide: two files of the same name deleted from different
// folders in the same second must not overwrite one another.
func TestTrashNamesDoNotCollide(t *testing.T) {
	h := newHarness(t)
	mkdir(t, filepath.Join(h.homes["alice"], "other"))
	writeFile(t, filepath.Join(h.homes["alice"], "other", "report.txt"), "the other one")
	if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	for _, p := range []string{"docs/report.txt", "other/report.txt"} {
		resp := h.do("DELETE", "/remote.php/dav/files/alice/"+p, "alice", alicePassword, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete %s: status = %d", p, resp.StatusCode)
		}
	}

	entries, err := store.ListTrash(t.Context(), h.db, aliceID(t, h))
	if err != nil {
		t.Fatalf("list trash: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected two trash entries, got %d", len(entries))
	}
	if entries[0].Name == entries[1].Name {
		t.Errorf("both deletions share the trash name %q", entries[0].Name)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.OriginalPath] = true
	}
	for _, want := range []string{"docs/report.txt", "other/report.txt"} {
		if !seen[want] {
			t.Errorf("no trash entry records %s", want)
		}
	}
	// Both files are physically present.
	files, _ := os.ReadDir(filepath.Join(h.homes["alice"], fsx.TrashDir))
	if len(files) != 2 {
		t.Errorf("expected two files in the trash directory, got %d", len(files))
	}
}
