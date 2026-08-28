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
