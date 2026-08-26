package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/config"
	"github.com/poitch/mirage/internal/store"
)

const (
	alicePassword = "alice-password-1"
	bobPassword   = "bob-password-1"
)

type harness struct {
	t      *testing.T
	http   *httptest.Server
	db     *store.DB
	server *Server
	homes  map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()

	homes := map[string]string{
		"alice": filepath.Join(base, "alice"),
		"bob":   filepath.Join(base, "bob"),
	}
	mkdir(t, filepath.Join(homes["alice"], "docs", "nested"))
	mkdir(t, homes["bob"])
	writeFile(t, filepath.Join(homes["alice"], "hello.txt"), "hello from alice")
	writeFile(t, filepath.Join(homes["alice"], "docs", "report.txt"), "quarterly report")
	writeFile(t, filepath.Join(homes["alice"], "docs", "nested", "deep.txt"), "deep")
	writeFile(t, filepath.Join(homes["bob"], "secret.txt"), "bob's private data")

	cfg := config.Default()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.ExternalURL = "https://mirage.test"
	cfg.Database.Path = filepath.Join(base, "index.db")
	cfg.Storage.RescanInterval = 0
	for _, name := range []string{"alice", "bob"} {
		cfg.Users = append(cfg.Users, config.User{
			Username: name, DisplayName: strings.ToUpper(name[:1]) + name[1:],
			Home: homes[name], UID: os.Getuid(), GID: os.Getgid(),
		})
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	db, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mappings := make([]store.UserMapping, 0, len(cfg.Users))
	for _, u := range cfg.Users {
		mappings = append(mappings, store.UserMapping{
			Username: u.Username, DisplayName: u.DisplayName,
			Home: u.Home, UID: u.UID, GID: u.GID,
		})
	}
	if _, err := db.ReconcileUsers(ctx, mappings); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for name, password := range map[string]string{"alice": alicePassword, "bob": bobPassword} {
		u, _ := db.UserByName(ctx, name)
		hash, _ := auth.HashPassword(password)
		if err := db.SetPasswordHash(ctx, u.ID, hash); err != nil {
			t.Fatalf("set password: %v", err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(ctx, &cfg, db, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.storage.Close() })

	if err := srv.scanner.ScanAll(ctx); err != nil {
		t.Fatalf("initial scan: %v", err)
	}

	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return &harness{t: t, http: ts, db: db, server: srv, homes: homes}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// do issues a request, optionally authenticated.
func (h *harness) do(method, path, user, password string, body string, headers map[string]string) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.http.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Redirects are not followed: a traversal attempt that the mux normalises
	// into a redirect should be observed as the redirect it is.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (h *harness) propfind(path, user, password, depth string) *http.Response {
	h.t.Helper()
	return h.do("PROPFIND", path, user, password, "", map[string]string{"Depth": depth})
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// multistatus is a minimal parse of a 207 response, used to assert on structure
// rather than on exact bytes.
type multistatusDoc struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				Inner string `xml:",innerxml"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

func parseMultistatus(t *testing.T, body string) multistatusDoc {
	t.Helper()
	var doc multistatusDoc
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse multistatus: %v\nbody:\n%s", err, body)
	}
	return doc
}

// prop fetches one property of one resource, for assertions that care about a
// single value rather than the shape of a listing.
func (h *harness) prop(t *testing.T, path, space, local string) string {
	t.Helper()
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:oc="` + space + `">` +
		`<d:prop><oc:` + local + `/></d:prop></d:propfind>`
	if space == "DAV:" {
		body = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:` + local + `/></d:prop></d:propfind>`
	}
	resp := h.do("PROPFIND", path, "alice", alicePassword, body, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND %s: status = %d", path, resp.StatusCode)
	}
	raw := readBody(t, resp)
	re := regexp.MustCompile(`<(?:d|oc):` + regexp.QuoteMeta(local) + `>([^<]*)<`)
	m := re.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	return m[1]
}

func (h *harness) etag(t *testing.T, path string) string {
	t.Helper()
	return h.prop(t, path, "DAV:", "getetag")
}

func (h *harness) fileID(t *testing.T, path string) string {
	t.Helper()
	return h.prop(t, path, "http://owncloud.org/ns", "fileid")
}

func aliceID(t *testing.T, h *harness) int64 {
	t.Helper()
	u, err := h.db.UserByName(context.Background(), "alice")
	if err != nil {
		t.Fatalf("look up alice: %v", err)
	}
	return u.ID
}

func (d multistatusDoc) hrefs() []string {
	out := make([]string, 0, len(d.Responses))
	for _, r := range d.Responses {
		out = append(out, r.Href)
	}
	return out
}

func TestPropfindListsDirectory(t *testing.T) {
	h := newHarness(t)
	resp := h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "1")
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	doc := parseMultistatus(t, readBody(t, resp))

	got := strings.Join(doc.hrefs(), " ")
	// Depth 1 is the collection itself plus its immediate children, and no more.
	for _, want := range []string{
		"/remote.php/dav/files/alice/",
		"/remote.php/dav/files/alice/docs/",
		"/remote.php/dav/files/alice/hello.txt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listing is missing %q; got %v", want, doc.hrefs())
		}
	}
	if strings.Contains(got, "nested/deep.txt") {
		t.Errorf("Depth 1 returned a grandchild; got %v", doc.hrefs())
	}
	// Directories carry a trailing slash so clients can tell a collection from
	// a file before parsing resourcetype.
	if !strings.HasSuffix(doc.Responses[0].Href, "/") {
		t.Errorf("collection href %q has no trailing slash", doc.Responses[0].Href)
	}
}

func TestPropfindDepths(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		depth string
		want  int
	}{
		{"0", 1}, // the collection alone
		{"1", 3}, // plus docs/ and hello.txt
		// The whole tree: root, docs/, docs/nested/, and the three files.
		{"infinity", 6},
	}
	for _, tc := range tests {
		resp := h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, tc.depth)
		doc := parseMultistatus(t, readBody(t, resp))
		if len(doc.Responses) != tc.want {
			t.Errorf("Depth %s returned %d responses, want %d: %v",
				tc.depth, len(doc.Responses), tc.want, doc.hrefs())
		}
	}
}

func TestPropfindRejectsBadDepth(t *testing.T) {
	h := newHarness(t)
	resp := h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "2")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPropfindReportsUnknownPropsAsMissing: clients compare what they asked for
// against what came back, so an unsupported property has to be reported absent
// rather than silently dropped.
func TestPropfindReportsUnknownPropsAsMissing(t *testing.T) {
	h := newHarness(t)
	body := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:" xmlns:x="http://example.com/ns">
	  <d:prop><d:getetag/><x:nonsense/></d:prop></d:propfind>`
	resp := h.do("PROPFIND", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, body, map[string]string{"Depth": "0"})
	doc := parseMultistatus(t, readBody(t, resp))

	if len(doc.Responses) != 1 {
		t.Fatalf("got %d responses, want 1", len(doc.Responses))
	}
	var found, missing bool
	for _, ps := range doc.Responses[0].Propstat {
		switch {
		case strings.Contains(ps.Status, "200") && strings.Contains(ps.Prop.Inner, "getetag"):
			found = true
		case strings.Contains(ps.Status, "404") && strings.Contains(ps.Prop.Inner, "nonsense"):
			missing = true
		}
	}
	if !found {
		t.Error("getetag was not reported as present")
	}
	if !missing {
		t.Error("the unknown property was not reported as 404")
	}
}

func TestGetFile(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/remote.php/dav/files/alice/docs/report.txt",
		"alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); !strings.HasPrefix(etag, `"`) {
		t.Errorf("ETag = %q, want a quoted value", etag)
	}
	if resp.Header.Get("OC-FileId") == "" {
		t.Error("OC-FileId header is missing")
	}
	if got := readBody(t, resp); got != "quarterly report" {
		t.Errorf("body = %q, want %q", got, "quarterly report")
	}
}

func TestGetSupportsRange(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/remote.php/dav/files/alice/docs/report.txt",
		"alice", alicePassword, "", map[string]string{"Range": "bytes=0-8"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := readBody(t, resp); got != "quarterly" {
		t.Errorf("body = %q, want %q", got, "quarterly")
	}
}

func TestGetDirectoryIsRejected(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/remote.php/dav/files/alice/docs/", "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestWritePermissionsAreAdvertised keeps the advertised permissions and the
// accepted methods from drifting apart: a client believes oc:permissions, and
// offers exactly the operations it claims.
func TestWritePermissionsAreAdvertised(t *testing.T) {
	h := newHarness(t)

	resp := h.propfind("/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "0")
	if body := readBody(t, resp); !strings.Contains(body, "<oc:permissions>GDNVW</oc:permissions>") {
		t.Error("a writable file should advertise W")
	}
	resp = h.propfind("/remote.php/dav/files/alice/docs/", "alice", alicePassword, "0")
	if body := readBody(t, resp); !strings.Contains(body, "<oc:permissions>GDNVCK</oc:permissions>") {
		t.Error("a writable directory should advertise C and K")
	}

	resp = h.do("OPTIONS", "/remote.php/dav/files/alice/", "alice", alicePassword, "", nil)
	allow := resp.Header.Get("Allow")
	resp.Body.Close()
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH"} {
		if !strings.Contains(allow, method) {
			t.Errorf("Allow = %q, missing %s", allow, method)
		}
	}
	// Class 2 means locking, which is not implemented, so it must not be
	// claimed - and Nextcloud does not claim it either.
	if dav := resp.Header.Get("DAV"); strings.Contains(dav, "2") {
		t.Errorf("DAV = %q, must not advertise class 2 without locking", dav)
	}
}

func TestPutCreatesAndReplaces(t *testing.T) {
	h := newHarness(t)
	target := "/remote.php/dav/files/alice/docs/uploaded.txt"

	resp := h.do(http.MethodPut, target, "alice", alicePassword, "first version", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("OC-FileId") == "" {
		t.Error("create: no OC-FileId header")
	}
	firstETag := resp.Header.Get("ETag")
	resp.Body.Close()

	onDisk := filepath.Join(h.homes["alice"], "docs", "uploaded.txt")
	if got, err := os.ReadFile(onDisk); err != nil || string(got) != "first version" {
		t.Fatalf("file on disk = %q, %v; want %q", got, err, "first version")
	}

	// Replacing an existing file is a 204, not a 201.
	resp = h.do(http.MethodPut, target, "alice", alicePassword, "second version, longer", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("replace: status = %d, want 204", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == firstETag {
		t.Error("replacing the content did not change the ETag")
	}
	resp.Body.Close()

	resp = h.do(http.MethodGet, target, "alice", alicePassword, "", nil)
	if got := readBody(t, resp); got != "second version, longer" {
		t.Errorf("round trip returned %q", got)
	}
}

// TestPutPropagatesToRoot is the sync-critical property on the write path: a
// client that sees an unchanged root ETag never looks deeper.
func TestPutPropagatesToRoot(t *testing.T) {
	h := newHarness(t)
	rootBefore := h.etag(t, "/remote.php/dav/files/alice/")
	deepBefore := h.etag(t, "/remote.php/dav/files/alice/docs/nested/")

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/docs/nested/new.txt",
		"alice", alicePassword, "content", nil)
	resp.Body.Close()

	if h.etag(t, "/remote.php/dav/files/alice/docs/nested/") == deepBefore {
		t.Error("the containing directory ETag did not change")
	}
	if h.etag(t, "/remote.php/dav/files/alice/") == rootBefore {
		t.Error("the root ETag did not change; clients would never see the upload")
	}
}

func TestPutPreservesModificationTime(t *testing.T) {
	h := newHarness(t)
	want := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Second)

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/old.txt",
		"alice", alicePassword, "aged", map[string]string{
			"X-OC-Mtime": strconv.FormatInt(want.Unix(), 10),
		})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// Clients look for this to confirm the timestamp survived.
	if got := resp.Header.Get("X-OC-MTime"); got != "accepted" {
		t.Errorf("X-OC-MTime = %q, want accepted", got)
	}
	resp.Body.Close()

	info, err := os.Stat(filepath.Join(h.homes["alice"], "old.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Truncate(time.Second).Equal(want) {
		t.Errorf("mtime on disk = %v, want %v", info.ModTime(), want)
	}
}

// TestPutVerifiesChecksum: a corrupted upload must not replace a good file.
func TestPutVerifiesChecksum(t *testing.T) {
	h := newHarness(t)
	body := "verified content"
	sum := sha1.Sum([]byte(body))
	correct := "SHA1:" + hex.EncodeToString(sum[:])

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/sums.txt",
		"alice", alicePassword, body, map[string]string{"OC-Checksum": correct})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("matching checksum: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do(http.MethodPut, "/remote.php/dav/files/alice/sums.txt",
		"alice", alicePassword, "different content",
		map[string]string{"OC-Checksum": correct})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched checksum: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// The good version must survive the rejected upload.
	got, err := os.ReadFile(filepath.Join(h.homes["alice"], "sums.txt"))
	if err != nil || string(got) != body {
		t.Errorf("file on disk = %q, %v; a failed upload must not damage it", got, err)
	}
}

func TestPutRejectsBadTargets(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name, path string
		want       int
	}{
		// RFC 4918: writing into a collection that does not exist is a
		// conflict, which tells the client to MKCOL first.
		{"missing parent", "/remote.php/dav/files/alice/nope/deep/file.txt", http.StatusConflict},
		{"over a directory", "/remote.php/dav/files/alice/docs", http.StatusMethodNotAllowed},
		{"the collection root", "/remote.php/dav/files/alice/", http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPut, tc.path, "alice", alicePassword, "x", nil)
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			resp.Body.Close()
		})
	}
}

func TestPutEnforcesQuota(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	alice, _ := h.db.UserByName(ctx, "alice")
	// alice already holds 36 bytes, so allow only a little more.
	if _, err := h.db.ExecContext(ctx, `UPDATE users SET quota = ? WHERE id = ?`, 50, alice.ID); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/small.txt",
		"alice", alicePassword, "0123456789", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("within quota: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do(http.MethodPut, "/remote.php/dav/files/alice/big.txt",
		"alice", alicePassword, strings.Repeat("x", 500), nil)
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("over quota: status = %d, want 507", resp.StatusCode)
	}
	resp.Body.Close()

	// A rejected upload must leave nothing behind, not even a partial file.
	if _, err := os.Stat(filepath.Join(h.homes["alice"], "big.txt")); !os.IsNotExist(err) {
		t.Error("an over-quota upload left a file on disk")
	}
}

func TestMkcolAndDelete(t *testing.T) {
	h := newHarness(t)

	resp := h.do("MKCOL", "/remote.php/dav/files/alice/newdir", "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
	if info, err := os.Stat(filepath.Join(h.homes["alice"], "newdir")); err != nil || !info.IsDir() {
		t.Fatalf("directory not created on disk: %v", err)
	}

	resp = h.do("MKCOL", "/remote.php/dav/files/alice/newdir", "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("MKCOL over an existing directory: status = %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("MKCOL", "/remote.php/dav/files/alice/a/b/c", "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("MKCOL with a missing parent: status = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	rootBefore := h.etag(t, "/remote.php/dav/files/alice/")
	resp = h.do(http.MethodDelete, "/remote.php/dav/files/alice/docs", "alice", alicePassword, "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	if _, err := os.Stat(filepath.Join(h.homes["alice"], "docs")); !os.IsNotExist(err) {
		t.Error("deleted directory still on disk")
	}
	if _, err := store.NodeByPath(context.Background(), h.db, aliceID(t, h), "docs/report.txt"); err == nil {
		t.Error("deleting a directory left its contents in the index")
	}
	if h.etag(t, "/remote.php/dav/files/alice/") == rootBefore {
		t.Error("the root ETag did not change after a delete")
	}
}

// TestMovePreservesFileID is the reason file IDs exist. A client that sees a
// familiar ID at a new path renames locally; a new ID makes it delete and
// re-download the whole subtree.
func TestMovePreservesFileID(t *testing.T) {
	h := newHarness(t)
	before := h.fileID(t, "/remote.php/dav/files/alice/docs/report.txt")
	if before == "" {
		t.Fatal("no file ID before the move")
	}

	resp := h.do("MOVE", "/remote.php/dav/files/alice/docs/report.txt",
		"alice", alicePassword, "", map[string]string{
			"Destination": h.http.URL + "/remote.php/dav/files/alice/renamed.txt",
		})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MOVE: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	if after := h.fileID(t, "/remote.php/dav/files/alice/renamed.txt"); after != before {
		t.Errorf("file ID changed across a move: %q then %q", before, after)
	}
	if got, err := os.ReadFile(filepath.Join(h.homes["alice"], "renamed.txt")); err != nil {
		t.Errorf("moved file not on disk: %v", err)
	} else if string(got) != "quarterly report" {
		t.Errorf("moved file content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.homes["alice"], "docs", "report.txt")); !os.IsNotExist(err) {
		t.Error("the source still exists after a move")
	}
}

// TestMoveDirectoryKeepsDescendantIDs: renaming a folder must not look like
// deleting and recreating everything inside it.
func TestMoveDirectoryKeepsDescendantIDs(t *testing.T) {
	h := newHarness(t)
	deepBefore := h.fileID(t, "/remote.php/dav/files/alice/docs/nested/deep.txt")

	resp := h.do("MOVE", "/remote.php/dav/files/alice/docs",
		"alice", alicePassword, "", map[string]string{
			"Destination": h.http.URL + "/remote.php/dav/files/alice/archive",
		})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MOVE: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	after := h.fileID(t, "/remote.php/dav/files/alice/archive/nested/deep.txt")
	if after != deepBefore {
		t.Errorf("descendant file ID changed across a directory move: %q then %q", deepBefore, after)
	}
	// The old paths must be gone from the index, not merely shadowed.
	if _, err := store.NodeByPath(context.Background(), h.db, aliceID(t, h), "docs/nested/deep.txt"); err == nil {
		t.Error("the old path is still indexed after a move")
	}
}

func TestMoveOverwriteRules(t *testing.T) {
	h := newHarness(t)
	dst := h.http.URL + "/remote.php/dav/files/alice/hello.txt"

	resp := h.do("MOVE", "/remote.php/dav/files/alice/docs/report.txt",
		"alice", alicePassword, "", map[string]string{"Destination": dst, "Overwrite": "F"})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("Overwrite: F onto an existing file: status = %d, want 412", resp.StatusCode)
	}
	resp.Body.Close()

	// Overwrite defaults to T, and replacing is a 204 rather than a 201.
	resp = h.do("MOVE", "/remote.php/dav/files/alice/docs/report.txt",
		"alice", alicePassword, "", map[string]string{"Destination": dst})
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("overwriting move: status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCopyDuplicatesTree(t *testing.T) {
	h := newHarness(t)

	resp := h.do("COPY", "/remote.php/dav/files/alice/docs", "alice", alicePassword, "",
		map[string]string{"Destination": h.http.URL + "/remote.php/dav/files/alice/docs-copy"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("COPY: status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	got, err := os.ReadFile(filepath.Join(h.homes["alice"], "docs-copy", "nested", "deep.txt"))
	if err != nil || string(got) != "deep" {
		t.Fatalf("copied file = %q, %v", got, err)
	}
	// The original must still be there; COPY is not MOVE.
	if _, err := os.Stat(filepath.Join(h.homes["alice"], "docs", "nested", "deep.txt")); err != nil {
		t.Errorf("COPY removed the source: %v", err)
	}
	// A copy is new content, so it gets new IDs rather than sharing the
	// originals'.
	if h.fileID(t, "/remote.php/dav/files/alice/docs-copy/nested/deep.txt") ==
		h.fileID(t, "/remote.php/dav/files/alice/docs/nested/deep.txt") {
		t.Error("a copied file shares its file ID with the original")
	}
}

// TestWritesLeaveNoPartialFiles: uploads land through a temporary file, and
// nothing should ever observe one - not a client, not somebody on SMB.
func TestWritesLeaveNoPartialFiles(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		resp := h.do(http.MethodPut,
			"/remote.php/dav/files/alice/tmp"+strconv.Itoa(i)+".bin",
			"alice", alicePassword, strings.Repeat("x", 1000), nil)
		resp.Body.Close()
	}
	// One deliberate failure, to check the cleanup path too.
	resp := h.do(http.MethodPut, "/remote.php/dav/files/alice/bad.txt",
		"alice", alicePassword, "content", map[string]string{"OC-Checksum": "SHA1:" + strings.Repeat("0", 40)})
	resp.Body.Close()

	entries, err := os.ReadDir(h.homes["alice"])
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mirage-tmp-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(h.homes["alice"], "bad.txt")); !os.IsNotExist(err) {
		t.Error("a failed upload left the destination file behind")
	}
}

// TestWritesCannotEscapeTheAccount is the isolation guarantee applied to the
// write path, where a mistake would be far worse than a read leak.
func TestWritesCannotEscapeTheAccount(t *testing.T) {
	h := newHarness(t)

	t.Run("PUT into another account", func(t *testing.T) {
		resp := h.do(http.MethodPut, "/remote.php/dav/files/bob/planted.txt",
			"alice", alicePassword, "alice was here", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		resp.Body.Close()
		if _, err := os.Stat(filepath.Join(h.homes["bob"], "planted.txt")); !os.IsNotExist(err) {
			t.Fatal("alice wrote a file into bob's home directory")
		}
	})

	t.Run("MOVE with a destination in another account", func(t *testing.T) {
		resp := h.do("MOVE", "/remote.php/dav/files/alice/hello.txt",
			"alice", alicePassword, "", map[string]string{
				"Destination": h.http.URL + "/remote.php/dav/files/bob/stolen.txt",
			})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		resp.Body.Close()
		if _, err := os.Stat(filepath.Join(h.homes["bob"], "stolen.txt")); !os.IsNotExist(err) {
			t.Fatal("a move placed a file into bob's home directory")
		}
	})

	t.Run("MOVE with a traversing destination", func(t *testing.T) {
		resp := h.do("MOVE", "/remote.php/dav/files/alice/hello.txt",
			"alice", alicePassword, "", map[string]string{
				"Destination": h.http.URL + "/remote.php/dav/files/alice/..%2f..%2fescaped.txt",
			})
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
			t.Errorf("a traversing destination was accepted: status = %d", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("DELETE in another account", func(t *testing.T) {
		resp := h.do(http.MethodDelete, "/remote.php/dav/files/bob/secret.txt",
			"alice", alicePassword, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		resp.Body.Close()
		if _, err := os.Stat(filepath.Join(h.homes["bob"], "secret.txt")); err != nil {
			t.Fatalf("alice deleted a file from bob's home directory: %v", err)
		}
	})
}

func TestProppatchIsRefusedCleanly(t *testing.T) {
	h := newHarness(t)
	body := `<?xml version="1.0"?><d:propertyupdate xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
	  <d:set><d:prop><oc:favorite>1</oc:favorite></d:prop></d:set></d:propertyupdate>`
	resp := h.do("PROPPATCH", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, body, nil)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	if got := readBody(t, resp); !strings.Contains(got, "403 Forbidden") {
		t.Errorf("PROPPATCH did not report the property as forbidden; got:\n%s", got)
	}
}

// TestTenantIsolation is the guarantee from the project's goals, asserted at
// the HTTP boundary rather than only at the storage layer.
func TestTenantIsolation(t *testing.T) {
	h := newHarness(t)

	t.Run("cannot address another account", func(t *testing.T) {
		for _, path := range []string{
			"/remote.php/dav/files/bob/",
			"/remote.php/dav/files/bob/secret.txt",
		} {
			resp := h.do("PROPFIND", path, "alice", alicePassword, "", map[string]string{"Depth": "1"})
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s as alice: status = %d, want 404", path, resp.StatusCode)
			}
			body := readBody(t, resp)
			if strings.Contains(body, "private data") || strings.Contains(body, "secret.txt") {
				t.Errorf("%s leaked bob's data to alice", path)
			}
		}
	})

	t.Run("cannot traverse out", func(t *testing.T) {
		// Percent-encoded traversal reaches the handler intact and must be
		// rejected there; the literal form is normalised by the mux into a
		// redirect, which the cross-account check then refuses.
		for _, path := range []string{
			"/remote.php/dav/files/alice/..%2fbob%2fsecret.txt",
			"/remote.php/dav/files/alice/%2e%2e%2fbob%2fsecret.txt",
			"/remote.php/dav/files/alice/docs/%2e%2e/%2e%2e/bob/secret.txt",
		} {
			resp := h.do(http.MethodGet, path, "alice", alicePassword, "", nil)
			if resp.StatusCode == http.StatusOK {
				t.Errorf("%s returned 200; traversal was not blocked", path)
			}
			if strings.Contains(readBody(t, resp), "private data") {
				t.Errorf("%s leaked bob's data", path)
			}
		}
	})

	t.Run("symlink out of home is not followed", func(t *testing.T) {
		link := filepath.Join(h.homes["alice"], "bob-link")
		if err := os.Symlink(h.homes["bob"], link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		defer os.Remove(link)
		if err := h.server.scanner.ScanAll(context.Background()); err != nil {
			t.Fatalf("rescan: %v", err)
		}

		resp := h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "1")
		if body := readBody(t, resp); strings.Contains(body, "bob-link") {
			t.Error("a symlink pointing outside the home was indexed")
		}
		resp = h.do(http.MethodGet, "/remote.php/dav/files/alice/bob-link/secret.txt",
			"alice", alicePassword, "", nil)
		if resp.StatusCode == http.StatusOK {
			t.Error("a symlink out of the home was followed")
		}
		resp.Body.Close()
	})

	t.Run("bob still reaches his own files", func(t *testing.T) {
		resp := h.propfind("/remote.php/dav/files/bob/", "bob", bobPassword, "1")
		if resp.StatusCode != http.StatusMultiStatus {
			t.Fatalf("status = %d, want 207", resp.StatusCode)
		}
		if !strings.Contains(readBody(t, resp), "secret.txt") {
			t.Error("bob cannot see his own file")
		}
	})
}

func TestUnauthenticatedAccessIsRefused(t *testing.T) {
	h := newHarness(t)
	// Each entry is a path and the method a client would really use on it.
	for _, tc := range []struct{ method, path string }{
		{"PROPFIND", "/remote.php/dav/files/alice/"},
		{http.MethodGet, "/remote.php/dav/files/alice/hello.txt"},
		{http.MethodGet, "/ocs/v2.php/cloud/user"},
	} {
		path := tc.path
		resp := h.do(tc.method, path, "", "", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate header; clients would not retry", path)
		}
		resp.Body.Close()
	}
}

// TestLegacyWebDAVRoot covers the pre-DAV path desktop clients still probe.
func TestLegacyWebDAVRoot(t *testing.T) {
	h := newHarness(t)
	resp := h.propfind("/remote.php/webdav/", "alice", alicePassword, "1")
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	if !strings.Contains(readBody(t, resp), "hello.txt") {
		t.Error("legacy root did not list the user's files")
	}
}

func TestQuotaReportedInPropfindAndOCS(t *testing.T) {
	h := newHarness(t)
	resp := h.propfind("/remote.php/dav/files/alice/", "alice", alicePassword, "0")
	body := readBody(t, resp)

	// alice's three files total 36 bytes.
	if !strings.Contains(body, "<d:quota-used-bytes>36</d:quota-used-bytes>") {
		t.Errorf("quota-used-bytes not reported as 36; body:\n%s", body)
	}
	if !strings.Contains(body, "<d:quota-available-bytes>-3</d:quota-available-bytes>") {
		t.Error("an unlimited account should report -3 available bytes")
	}

	resp = h.do(http.MethodGet, "/ocs/v2.php/cloud/user?format=json", "alice", alicePassword, "", nil)
	if got := readBody(t, resp); !strings.Contains(got, `"used":36`) {
		t.Errorf("OCS user endpoint did not report usage; got:\n%s", got)
	}
}

func TestStatusAndCapabilitiesNeedNoAuth(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/status.php", "/ocs/v2.php/cloud/capabilities?format=json"} {
		resp := h.do(http.MethodGet, path, "", "", "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
