package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/poitch/mirage/internal/store"
)

type unifiedEntry struct {
	Title       string            `json:"title"`
	Subline     string            `json:"subline"`
	ResourceURL string            `json:"resourceUrl"`
	Icon        string            `json:"icon"`
	Attributes  map[string]string `json:"attributes"`
}

type unifiedResult struct {
	OCS struct {
		Meta struct {
			StatusCode int `json:"statuscode"`
		} `json:"meta"`
		Data struct {
			Name        string         `json:"name"`
			IsPaginated bool           `json:"isPaginated"`
			Entries     []unifiedEntry `json:"entries"`
			Cursor      *string        `json:"cursor"`
		} `json:"data"`
	} `json:"ocs"`
}

func (h *harness) unified(t *testing.T, term string, extra url.Values) unifiedResult {
	t.Helper()
	q := url.Values{"format": {"json"}, "term": {term}}
	for k, v := range extra {
		q[k] = v
	}
	resp := h.do("GET", "/ocs/v2.php/search/providers/files/search?"+q.Encode(),
		"alice", alicePassword, "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search %q: status = %d: %s", term, resp.StatusCode, body)
	}
	var out unifiedResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	return out
}

// TestUnifiedSearchProvidersIsOffered: the desktop client asks for this before
// it will show a search box at all, and an unhandled route was why searching
// from the desktop did nothing.
func TestUnifiedSearchProvidersIsOffered(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/ocs/v2.php/search/providers?format=json", "alice", alicePassword, "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		OCS struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(env.OCS.Data) != 1 || env.OCS.Data[0].ID != "files" {
		t.Fatalf("providers = %+v, want just the files provider", env.OCS.Data)
	}
}

func TestUnifiedSearchFindsFiles(t *testing.T) {
	h := newHarness(t)
	got := h.unified(t, "deep", nil)
	if len(got.OCS.Data.Entries) != 1 {
		t.Fatalf("entries = %+v, want deep.txt", got.OCS.Data.Entries)
	}
	e := got.OCS.Data.Entries[0]
	if e.Title != "deep.txt" {
		t.Errorf("title = %q, want deep.txt", e.Title)
	}
	// The folder is what tells two files of the same name apart in a list.
	if e.Subline != "/docs/nested" {
		t.Errorf("subline = %q, want /docs/nested", e.Subline)
	}
	if e.Attributes["path"] != "/docs/nested/deep.txt" {
		t.Errorf("path attribute = %q", e.Attributes["path"])
	}
	if e.Attributes["fileId"] == "" {
		t.Error("no fileId; the client matches results against its journal by id")
	}

	// The desktop client only reveals a result in the file manager when the URL
	// carries dir and scrollto; without them it silently opens a browser
	// instead, which is not what clicking a search result should do.
	u, err := url.Parse(e.ResourceURL)
	if err != nil {
		t.Fatalf("resourceUrl %q: %v", e.ResourceURL, err)
	}
	if got := u.Query().Get("dir"); got != "/docs/nested" {
		t.Errorf("dir = %q, want /docs/nested", got)
	}
	if got := u.Query().Get("scrollto"); got != "deep.txt" {
		t.Errorf("scrollto = %q, want deep.txt", got)
	}
}

// TestSearchResultAtTheRootIsRevealable: dir must be non-empty even at the top
// of the account, or the client falls back to the browser.
func TestSearchResultAtTheRootIsRevealable(t *testing.T) {
	h := newHarness(t)
	got := h.unified(t, "hello", nil)
	if len(got.OCS.Data.Entries) != 1 {
		t.Fatalf("entries = %+v, want hello.txt", got.OCS.Data.Entries)
	}
	u, err := url.Parse(got.OCS.Data.Entries[0].ResourceURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if dir := u.Query().Get("dir"); dir != "/" {
		t.Errorf("dir = %q for a file at the root, want /", dir)
	}
	if got := u.Query().Get("scrollto"); got != "hello.txt" {
		t.Errorf("scrollto = %q", got)
	}
}

// TestFilesPageExplainsWhereTheFileIs covers the fallback: the client opens a
// browser here only when the folder is not synced on the device.
func TestFilesPageExplainsWhereTheFileIs(t *testing.T) {
	h := newHarness(t)

	// Reached without credentials, because a browser opened from the client
	// carries none and this shows only what the URL already contained.
	resp := h.do("GET", "/index.php/apps/files/?dir=%2Fdocs%2Fnested&scrollto=deep.txt", "", "", "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "/docs/nested") || !strings.Contains(body, "deep.txt") {
		t.Errorf("the page does not say where the file is:\n%s", body)
	}

	// Hand-edited or bare, it still renders rather than erroring.
	plain := h.do("GET", "/index.php/apps/files/", "", "", "", nil)
	plainBody := readBody(t, plain)
	if plain.StatusCode != http.StatusOK {
		t.Errorf("bare page status = %d, want 200", plain.StatusCode)
	}
	if !strings.Contains(plainBody, "no web interface") {
		t.Errorf("the bare page does not explain itself:\n%s", plainBody)
	}
}

// TestFilesPageEscapesWhatItIsGiven: the page reflects the query string, so a
// name carrying markup must not become markup.
func TestFilesPageEscapesWhatItIsGiven(t *testing.T) {
	h := newHarness(t)
	q := url.Values{"dir": {"/docs"}, "scrollto": {`<script>alert(1)</script>`}}
	body := readBody(t, h.do("GET", "/index.php/apps/files/?"+q.Encode(), "", "", "", nil))
	if strings.Contains(body, "<script>alert") {
		t.Errorf("the page reflected markup unescaped:\n%s", body)
	}
}

// TestUnifiedSearchTreatsTheTermAsText: somebody typing a percent sign means
// that character, not "match anything".
func TestUnifiedSearchTreatsTheTermAsText(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.homes["alice"]+"/50% off.txt", "sale")
	if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	if got := h.unified(t, "50%", nil); len(got.OCS.Data.Entries) != 1 {
		t.Errorf("a term with a wildcard matched %d entries, want 1", len(got.OCS.Data.Entries))
	}
	// A bare wildcard is taken as the character itself, so it finds the one
	// name containing it rather than every file in the account.
	got := h.unified(t, "%", nil)
	if len(got.OCS.Data.Entries) != 1 {
		t.Fatalf("a bare %% matched %d entries, want only the name containing one",
			len(got.OCS.Data.Entries))
	}
	if !strings.Contains(got.OCS.Data.Entries[0].Title, "%") {
		t.Errorf("a bare %% matched %q, which has no percent sign", got.OCS.Data.Entries[0].Title)
	}
}

func TestUnifiedSearchPaginates(t *testing.T) {
	h := newHarness(t)
	for i := range 7 {
		writeFile(t, fmt.Sprintf("%s/paged-%02d.txt", h.homes["alice"], i), "x")
	}
	if err := h.server.scanner.ScanAll(t.Context(), "test"); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	first := h.unified(t, "paged", url.Values{"limit": {"3"}})
	if len(first.OCS.Data.Entries) != 3 {
		t.Fatalf("first page had %d entries, want 3", len(first.OCS.Data.Entries))
	}
	if first.OCS.Data.Cursor == nil {
		t.Fatal("no cursor on a page that has more after it")
	}

	seen := map[string]bool{}
	for _, e := range first.OCS.Data.Entries {
		seen[e.Title] = true
	}

	second := h.unified(t, "paged", url.Values{"limit": {"3"}, "cursor": {*first.OCS.Data.Cursor}})
	if len(second.OCS.Data.Entries) != 3 {
		t.Fatalf("second page had %d entries, want 3", len(second.OCS.Data.Entries))
	}
	for _, e := range second.OCS.Data.Entries {
		if seen[e.Title] {
			t.Errorf("%s appeared on both pages", e.Title)
		}
		seen[e.Title] = true
	}

	last := h.unified(t, "paged", url.Values{"limit": {"3"}, "cursor": {*second.OCS.Data.Cursor}})
	if len(last.OCS.Data.Entries) != 1 {
		t.Errorf("last page had %d entries, want the remaining 1", len(last.OCS.Data.Entries))
	}
	// Null rather than an empty string, or a client reads it as a valid cursor
	// and keeps asking.
	if last.OCS.Data.Cursor != nil {
		t.Errorf("cursor = %q on the final page, want null", *last.OCS.Data.Cursor)
	}
}

// TestUnifiedSearchIsScopedToTheAccount is the isolation assertion: this
// endpoint takes no path, so nothing but the query itself confines it.
func TestUnifiedSearchIsScopedToTheAccount(t *testing.T) {
	h := newHarness(t)
	got := h.unified(t, "secret", nil)
	if len(got.OCS.Data.Entries) != 0 {
		t.Errorf("alice's search returned bob's files: %+v", got.OCS.Data.Entries)
	}
}

func TestUnifiedSearchRequiresAuth(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/ocs/v2.php/search/providers",
		"/ocs/v2.php/search/providers/files/search?term=hello",
	} {
		resp := h.do("GET", path, "", "", "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestUnifiedSearchRejectsUnknownProviders(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/ocs/v2.php/search/providers/calendar/search?term=x&format=json",
		"alice", alicePassword, "", nil)
	readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestOpenFileRedirectsToTheFile: a search result has to link somewhere, and a
// link that 404s is worse than none.
func TestOpenFileRedirectsToTheFile(t *testing.T) {
	h := newHarness(t)
	got := h.unified(t, "deep", nil)
	id := got.OCS.Data.Entries[0].Attributes["fileId"]

	resp := h.do("GET", "/index.php/f/"+id, "alice", alicePassword, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/docs/nested/deep.txt") {
		t.Errorf("Location = %q, want the file's WebDAV path", loc)
	}
}

// nodeIDFor finds the index id of a path belonging to a user.
func nodeIDFor(t *testing.T, h *harness, userID int64, path string) (int64, error) {
	t.Helper()
	n, err := store.NodeByPath(t.Context(), h.db, userID, path)
	if err != nil {
		return 0, err
	}
	return n.ID, nil
}

// TestOpenFileCannotReachAnotherAccount: the id is the only input, so nothing
// but the lookup confines it.
func TestOpenFileCannotReachAnotherAccount(t *testing.T) {
	h := newHarness(t)
	bob, err := h.db.UserByName(t.Context(), "bob")
	if err != nil {
		t.Fatalf("look up bob: %v", err)
	}
	secret, err := nodeIDFor(t, h, bob.ID, "secret.txt")
	if err != nil {
		t.Fatalf("find bob's file: %v", err)
	}

	resp := h.do("GET", fmt.Sprintf("/index.php/f/%d", secret), "alice", alicePassword, "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(body, "secret.txt") {
		t.Errorf("the refusal leaked the path: %s", body)
	}
}
