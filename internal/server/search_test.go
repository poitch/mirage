package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// searchBody builds the request the Nextcloud clients send, which is the shape
// the parser has to cope with rather than a convenient one.
func searchBody(scope, literal string, limit int) string {
	limitEl := ""
	if limit > 0 {
		limitEl = fmt.Sprintf("<d:limit><d:nresults>%d</d:nresults></d:limit>", limit)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<d:searchrequest xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:basicsearch>
    <d:select><d:prop><oc:fileid/><d:displayname/><d:getcontentlength/><d:resourcetype/></d:prop></d:select>
    <d:from><d:scope><d:href>%s</d:href><d:depth>infinity</d:depth></d:scope></d:from>
    <d:where><d:like><d:prop><d:displayname/></d:prop><d:literal>%s</d:literal></d:like></d:where>
    <d:orderby/>
    %s
  </d:basicsearch>
</d:searchrequest>`, scope, literal, limitEl)
}

func (h *harness) search(scope, literal string, limit int, user, password string) *http.Response {
	h.t.Helper()
	return h.do("SEARCH", "/remote.php/dav", user, password,
		searchBody(scope, literal, limit),
		map[string]string{"Content-Type": "text/xml"})
}

func TestSearchFindsFilesByName(t *testing.T) {
	h := newHarness(t)
	resp := h.search("/files/alice", "%deep%", 0, "alice", alicePassword)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	doc := parseMultistatus(t, readBody(t, resp))
	hrefs := doc.hrefs()
	if len(hrefs) != 1 || !strings.HasSuffix(hrefs[0], "/docs/nested/deep.txt") {
		t.Fatalf("search for deep returned %v, want just docs/nested/deep.txt", hrefs)
	}
}

// TestSearchIsScopedToTheRequestedFolder: the iOS app searches from whatever
// folder is open, and results from elsewhere would be wrong rather than merely
// noisy.
func TestSearchIsScopedToTheRequestedFolder(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.homes["alice"]+"/report-top.txt", "top level")
	if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	all := parseMultistatus(t, readBody(t, h.search("/files/alice", "%report%", 0, "alice", alicePassword)))
	if len(all.hrefs()) != 2 {
		t.Fatalf("unscoped search returned %v, want both reports", all.hrefs())
	}

	scoped := parseMultistatus(t, readBody(t, h.search("/files/alice/docs", "%report%", 0, "alice", alicePassword)))
	hrefs := scoped.hrefs()
	if len(hrefs) != 1 || !strings.HasSuffix(hrefs[0], "/docs/report.txt") {
		t.Fatalf("search scoped to docs returned %v, want only docs/report.txt", hrefs)
	}
}

// TestSearchCannotReachAnotherAccount is the isolation assertion for this
// endpoint: SEARCH names its own scope in the body, so the path-based check
// that protects every other endpoint does not apply here.
func TestSearchCannotReachAnotherAccount(t *testing.T) {
	h := newHarness(t)
	for _, scope := range []string{
		"/files/bob",
		"/files/bob/",
		"/remote.php/dav/files/bob",
		"/files/alice/../bob",
	} {
		resp := h.search(scope, "%secret%", 0, "alice", alicePassword)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("SEARCH scope %q = %d, want 404", scope, resp.StatusCode)
		}
		if strings.Contains(body, "secret.txt") {
			t.Errorf("SEARCH scope %q leaked bob's file", scope)
		}
	}
}

func TestSearchHonoursLimit(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		writeFile(t, fmt.Sprintf("%s/note-%d.txt", h.homes["alice"], i), "n")
	}
	if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	doc := parseMultistatus(t, readBody(t, h.search("/files/alice", "%note%", 2, "alice", alicePassword)))
	if len(doc.hrefs()) != 2 {
		t.Fatalf("limit 2 returned %d results: %v", len(doc.hrefs()), doc.hrefs())
	}
}

// TestSearchHonoursEscapedWildcards: a client looking for a name that itself
// contains a percent sign escapes it, and must not then match everything.
func TestSearchHonoursEscapedWildcards(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.homes["alice"]+"/50% off.txt", "sale")
	writeFile(t, h.homes["alice"]+"/50 off.txt", "no sale")
	if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	doc := parseMultistatus(t, readBody(t, h.search("/files/alice", `%50\%%`, 0, "alice", alicePassword)))
	hrefs := doc.hrefs()
	if len(hrefs) != 1 || !strings.Contains(hrefs[0], "off") {
		t.Fatalf(`search for %%50\%%%% returned %v, want only the file with a percent in its name`, hrefs)
	}
}

// TestSearchTakesAPlainTermAsASubstring: a client that sends no wildcards still
// means "contains", which is what a search box does.
func TestSearchTakesAPlainTermAsASubstring(t *testing.T) {
	h := newHarness(t)
	doc := parseMultistatus(t, readBody(t, h.search("/files/alice", "deep", 0, "alice", alicePassword)))
	if len(doc.hrefs()) != 1 {
		t.Fatalf("plain term returned %v, want docs/nested/deep.txt", doc.hrefs())
	}
}

// TestSearchRejectsQueriesItCannotAnswer: answering an unsupported query with
// an empty multistatus would read as "no such file", which is worse than an
// error the client can report.
func TestSearchRejectsQueriesItCannotAnswer(t *testing.T) {
	h := newHarness(t)
	body := `<?xml version="1.0"?>
<d:searchrequest xmlns:d="DAV:"><d:basicsearch>
  <d:select><d:prop><d:displayname/></d:prop></d:select>
  <d:from><d:scope><d:href>/files/alice</d:href><d:depth>infinity</d:depth></d:scope></d:from>
  <d:where><d:gt><d:prop><d:getlastmodified/></d:prop><d:literal>2024-01-01</d:literal></d:gt></d:where>
</d:basicsearch></d:searchrequest>`
	resp := h.do("SEARCH", "/remote.php/dav", "alice", alicePassword, body,
		map[string]string{"Content-Type": "text/xml"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestSearchRequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do("SEARCH", "/remote.php/dav", "", "", searchBody("/files/alice", "%hello%", 0), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestSearchReturnsRequestedProperties: the client matches results against the
// journal by fileid, so a result without one is unusable.
func TestSearchReturnsRequestedProperties(t *testing.T) {
	h := newHarness(t)
	body := readBody(t, h.search("/files/alice", "%hello%", 0, "alice", alicePassword))
	for _, want := range []string{"<oc:fileid>", "<d:displayname>", "<d:getcontentlength>"} {
		if !strings.Contains(body, want) {
			t.Errorf("search response is missing %s:\n%s", want, body)
		}
	}
}

func TestSearchMatchesDirectories(t *testing.T) {
	h := newHarness(t)
	doc := parseMultistatus(t, readBody(t, h.search("/files/alice", "%nested%", 0, "alice", alicePassword)))
	hrefs := doc.hrefs()
	sort.Strings(hrefs)
	if len(hrefs) != 1 || !strings.HasSuffix(hrefs[0], "/docs/nested/") {
		t.Fatalf("search for nested returned %v, want the directory with a trailing slash", hrefs)
	}
}
