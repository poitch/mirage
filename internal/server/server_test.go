package server

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestReadOnlyRefusesWrites keeps the advertised permissions and the accepted
// methods from drifting apart: while oc:permissions says read-only, a write
// must actually be refused.
func TestReadOnlyRefusesWrites(t *testing.T) {
	h := newHarness(t)
	for _, method := range []string{http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "PROPPATCH"} {
		resp := h.do(method, "/remote.php/dav/files/alice/new.txt", "alice", alicePassword, "x", nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "PROPFIND") {
			t.Errorf("%s: Allow = %q, want it to advertise PROPFIND", method, allow)
		}
		resp.Body.Close()
	}

	resp := h.propfind("/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "0")
	if body := readBody(t, resp); !strings.Contains(body, "<oc:permissions>G</oc:permissions>") {
		t.Error("read-only server did not advertise read-only permissions")
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
