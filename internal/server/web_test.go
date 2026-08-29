package server

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/poitch/mirage/internal/store"
)

// webClient signs in to the browser view and keeps its cookie.
func webClient(t *testing.T, h *harness, username, password string) *http.Client {
	t.Helper()
	jar := newCookieJar(t)

	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.PostForm(h.http.URL+"/web/login", url.Values{
		"username": {username}, "password": {password}, "next": {"/web/"},
	})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in: status = %d, want 303", resp.StatusCode)
	}
	return c
}

func (h *harness) webGet(t *testing.T, c *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := c.Get(h.http.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp.StatusCode, readBody(t, resp)
}

func TestWebRequiresSignIn(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/web/",
		"/web/download/hello.txt",
		"/index.php/apps/files/?dir=/docs&scrollto=report.txt",
	} {
		resp := h.do("GET", path, "", "", "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s = %d, want a redirect to sign in", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/web/login") {
			t.Errorf("GET %s redirected to %q, want the sign-in page", path, loc)
		}
	}
}

func TestWebBrowsesTheAccount(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)

	status, body := h.webGet(t, c, "/web/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{"hello.txt", "docs"} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing is missing %q", want)
		}
	}
	// Folders link to the listing, files to the download.
	if !strings.Contains(body, `/web/?path=docs`) {
		t.Errorf("no link into docs/:\n%s", body)
	}
	if !strings.Contains(body, `/web/download/hello.txt`) {
		t.Errorf("no download link for hello.txt")
	}
}

func TestWebDownloadsAFile(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)

	resp, err := c.Get(h.http.URL + "/web/download/docs/nested/deep.txt")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "deep" {
		t.Errorf("body = %q, want the file's contents", body)
	}
	// Never inline: an HTML file served inline would run as a page on this
	// origin, with the session cookie attached.
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("downloads are not marked nosniff")
	}
}

// TestWebCannotReachAnotherAccount is the isolation assertion for the browser
// view: a session names no account in the URL, so only the lookup confines it.
func TestWebCannotReachAnotherAccount(t *testing.T) {
	h := newHarness(t)
	// Redirects are followed here, because the mux normalises a traversal into
	// a redirect and what matters is where it lands, not the hop.
	c := webClient(t, h, "alice", alicePassword)
	c.CheckRedirect = nil

	for _, path := range []string{
		"/web/download/secret.txt",
		"/web/download/../bob/secret.txt",
		"/web/download/%2e%2e/bob/secret.txt",
		"/web/?path=../bob",
		"/web/?path=/etc",
		"/index.php/apps/files/?dir=../bob&scrollto=secret.txt",
	} {
		resp, err := c.Get(h.http.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readBody(t, resp)
		if strings.Contains(body, "bob's private data") {
			t.Errorf("GET %s served another account's file", path)
		}
		// A listing renders each name as link text; alice's own pages never
		// contain this one.
		if strings.Contains(body, ">secret.txt<") {
			t.Errorf("GET %s listed another account's file", path)
		}
	}
}

// TestWebHighlightsASearchResult is what the whole page is for: a client that
// cannot open a result locally sends the person here, and the file has to be
// findable on the page when they arrive.
func TestWebHighlightsASearchResult(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)

	status, body := h.webGet(t, c, "/index.php/apps/files/?dir=%2Fdocs%2Fnested&scrollto=deep.txt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "deep.txt") {
		t.Fatalf("the folder listing does not contain the file:\n%s", body)
	}
	if !strings.Contains(body, `class="hit"`) {
		t.Errorf("the named file was not marked on the page:\n%s", body)
	}
}

func TestWebShowsBreadcrumbs(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	_, body := h.webGet(t, c, "/web/?path=docs/nested")
	for _, want := range []string{`href="/web/"`, `href="/web/?path=docs"`, "nested"} {
		if !strings.Contains(body, want) {
			t.Errorf("breadcrumbs are missing %q:\n%s", want, body)
		}
	}
}

// TestWebLoginDoesNotRedirectOffSite: next comes from the query string, so
// without a check the sign-in page forwards anyone who follows a crafted link.
func TestWebLoginDoesNotRedirectOffSite(t *testing.T) {
	h := newHarness(t)
	for _, next := range []string{"https://evil.example/", "//evil.example/", "javascript:alert(1)"} {
		jar := newCookieJar(t)
		c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := c.PostForm(h.http.URL+"/web/login", url.Values{
			"username": {"alice"}, "password": {alicePassword}, "next": {next},
		})
		if err != nil {
			t.Fatalf("sign in: %v", err)
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
			t.Errorf("next=%q sent the browser to %q", next, loc)
		}
	}
}

func TestWebRejectsBadCredentials(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/web/login", "", "", "username=alice&password=wrong-password",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(body, "Wrong username or password") {
		t.Errorf("no error shown on the page")
	}
}

// TestWebSignOutEndsTheSession
func TestWebSignOutEndsTheSession(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	if status, _ := h.webGet(t, c, "/web/"); status != http.StatusOK {
		t.Fatalf("not signed in to begin with")
	}

	// The CSRF token is on the page.
	_, page := h.webGet(t, c, "/web/")
	token := betweenMarkers(page, `name="csrf" value="`, `"`)
	if token == "" {
		t.Fatal("no CSRF token on the page")
	}
	resp, err := c.PostForm(h.http.URL+"/web/logout", url.Values{"csrf": {token}})
	if err != nil {
		t.Fatalf("sign out: %v", err)
	}
	resp.Body.Close()

	if status, _ := h.webGet(t, c, "/web/"); status != http.StatusSeeOther {
		t.Errorf("still signed in after signing out: status = %d", status)
	}
}

// TestWebSessionEndsWhenTheAccountIsDisabled: a session must not outlive the
// account's right to use it.
func TestWebSessionEndsWhenTheAccountIsDisabled(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)

	u, err := h.db.UserByName(t.Context(), "alice")
	if err != nil {
		t.Fatalf("look up alice: %v", err)
	}
	if err := h.db.SetDisabled(t.Context(), u.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if status, _ := h.webGet(t, c, "/web/"); status != http.StatusSeeOther {
		t.Errorf("a disabled account kept its session: status = %d", status)
	}
}

// newCookieJar gives a client somewhere to keep its session cookie.
func newCookieJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return jar
}

func betweenMarkers(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestWebSearchFindsByName covers the box on the files page.
func TestWebSearchFindsByName(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)

	status, body := h.webGet(t, c, "/web/search?q=deep")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "deep.txt") {
		t.Errorf("the search did not find the file:\n%s", body)
	}
	// A result out of context is just a filename; the folder is what tells two
	// files of the same name apart.
	if !strings.Contains(body, "docs/nested") {
		t.Errorf("the result does not say which folder it is in:\n%s", body)
	}
	if strings.Contains(body, "hello.txt") {
		t.Errorf("the search returned files that do not match:\n%s", body)
	}
}

// TestWebSearchIsScopedToTheAccount: the query names no account.
func TestWebSearchIsScopedToTheAccount(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	_, body := h.webGet(t, c, "/web/search?q=secret")
	if strings.Contains(body, "secret.txt") {
		t.Errorf("alice's search returned bob's file:\n%s", body)
	}
}

func TestWebShowsThumbnails(t *testing.T) {
	h, id := harnessWithPhoto(t)
	c := webClient(t, h, "alice", alicePassword)

	_, body := h.webGet(t, c, "/web/")
	if !strings.Contains(body, "/web/thumb/") {
		t.Errorf("the listing shows no thumbnails:\n%s", body)
	}

	resp, err := c.Get(h.http.URL + "/web/thumb/" + fmt.Sprint(id))
	if err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	thumb := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail: status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("thumbnail content type = %q", ct)
	}
	if len(thumb) == 0 {
		t.Error("the thumbnail is empty")
	}
}

// TestWebThumbnailCannotReachAnotherAccount: a file id is the only input.
func TestWebThumbnailCannotReachAnotherAccount(t *testing.T) {
	h, _ := harnessWithPhoto(t)
	c := webClient(t, h, "alice", alicePassword)

	bob, err := h.db.UserByName(t.Context(), "bob")
	if err != nil {
		t.Fatalf("look up bob: %v", err)
	}
	bobPhoto, err := nodeIDFor(t, h, bob.ID, "private.jpg")
	if err != nil {
		t.Fatalf("find bob's photo: %v", err)
	}
	status, _ := h.webGet(t, c, "/web/thumb/"+fmt.Sprint(bobPhoto))
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for another account's photo", status)
	}
}

func TestWebTrashListsAndRestores(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)

	del := h.do("DELETE", "/remote.php/dav/files/alice/hello.txt", "alice", alicePassword, "", nil)
	del.Body.Close()

	_, body := h.webGet(t, c, "/web/trash")
	if !strings.Contains(body, "hello.txt") {
		t.Fatalf("the trash page does not list the deleted file:\n%s", body)
	}
	token := betweenMarkers(body, `name="entry" value="`, `"`)
	if token == "" {
		t.Fatal("no entry token on the trash page")
	}

	csrf := betweenMarkers(body, `name="csrf" value="`, `"`)
	resp, err := c.PostForm(h.http.URL+"/web/trash/restore",
		url.Values{"csrf": {csrf}, "entry": {token}})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("restore: status = %d, want 303", resp.StatusCode)
	}

	got := readBody(t, h.do("GET", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, "", nil))
	if got != "hello from alice" {
		t.Errorf("the restored file reads %q", got)
	}
	if _, after := h.webGet(t, c, "/web/trash"); strings.Contains(after, `name="entry"`) {
		t.Error("the entry is still in the trash after being restored")
	}
}

// TestWebTrashCannotTouchAnotherAccount: the form names an entry, and nothing
// but the lookup confines it.
func TestWebTrashCannotTouchAnotherAccount(t *testing.T) {
	h := newHarness(t)
	del := h.do("DELETE", "/remote.php/dav/files/bob/secret.txt", "bob", bobPassword, "", nil)
	del.Body.Close()

	entries, err := store.ListTrash(t.Context(), h.db, bobUserID(t, h))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one entry in bob's trash, got %v (%v)", entries, err)
	}

	c := webClient(t, h, "alice", alicePassword)
	_, page := h.webGet(t, c, "/web/trash")
	csrf := betweenMarkers(page, `name="csrf" value="`, `"`)

	for _, action := range []string{"/web/trash/restore", "/web/trash/delete"} {
		resp, err := c.PostForm(h.http.URL+action,
			url.Values{"csrf": {csrf}, "entry": {entries[0].Name}})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		resp.Body.Close()
	}
	// Bob's deletion is untouched.
	after, err := store.ListTrash(t.Context(), h.db, bobUserID(t, h))
	if err != nil || len(after) != 1 {
		t.Errorf("alice's request changed bob's trash: %v (%v)", after, err)
	}
}

func TestWebVersionsListAndRestore(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	id := h.fileIDInt(t, "hello.txt")
	h.saveFile(t, "hello.txt", "second draft")

	status, body := h.webGet(t, c, fmt.Sprintf("/web/versions/%d", id))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "Current") {
		t.Errorf("the versions page does not show the current file:\n%s", body)
	}
	token := betweenMarkers(body, `name="version" value="`, `"`)
	if token == "" {
		t.Fatalf("no version listed:\n%s", body)
	}

	// Downloading a version gives what was there before.
	got, err := c.Get(fmt.Sprintf("%s/web/versions/%d/%s", h.http.URL, id, token))
	if err != nil {
		t.Fatalf("download version: %v", err)
	}
	if body := readBody(t, got); body != "hello from alice" {
		t.Errorf("version download = %q, want the original", body)
	}

	csrf := betweenMarkers(body, `name="csrf" value="`, `"`)
	resp, err := c.PostForm(h.http.URL+"/web/versions/restore",
		url.Values{"csrf": {csrf}, "file": {fmt.Sprint(id)}, "version": {token}})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	resp.Body.Close()

	live := readBody(t, h.do("GET", "/remote.php/dav/files/alice/hello.txt",
		"alice", alicePassword, "", nil))
	if live != "hello from alice" {
		t.Errorf("after restoring, the file reads %q", live)
	}
}

func TestWebVersionsCannotReachAnotherAccount(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	put := h.do("PUT", "/remote.php/dav/files/bob/secret.txt", "bob", bobPassword, "changed", nil)
	put.Body.Close()

	bobFile, err := nodeIDFor(t, h, bobUserID(t, h), "secret.txt")
	if err != nil {
		t.Fatalf("find bob's file: %v", err)
	}
	status, body := h.webGet(t, c, fmt.Sprintf("/web/versions/%d", bobFile))
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if strings.Contains(body, "secret") {
		t.Errorf("the refusal leaked bob's filename:\n%s", body)
	}
}

func TestWebProfileChangesPassword(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)

	_, page := h.webGet(t, c, "/web/profile")
	csrf := betweenMarkers(page, `name="csrf" value="`, `"`)
	const newPassword = "a-much-better-password"

	resp, err := c.PostForm(h.http.URL+"/web/profile/password", url.Values{
		"csrf": {csrf}, "current": {alicePassword},
		"password": {newPassword}, "confirm": {newPassword},
	})
	if err != nil {
		t.Fatalf("change password: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Password changed") {
		t.Fatalf("no confirmation shown:\n%s", body)
	}

	// The new password works on the sync endpoints, and the old one does not.
	ok := h.do("PROPFIND", "/remote.php/dav/files/alice/", "alice", newPassword, "",
		map[string]string{"Depth": "0"})
	ok.Body.Close()
	if ok.StatusCode != http.StatusMultiStatus {
		t.Errorf("the new password does not work: status = %d", ok.StatusCode)
	}
	old := h.do("PROPFIND", "/remote.php/dav/files/alice/", "alice", alicePassword, "",
		map[string]string{"Depth": "0"})
	old.Body.Close()
	if old.StatusCode != http.StatusUnauthorized {
		t.Errorf("the old password still works: status = %d", old.StatusCode)
	}
}

// TestWebProfileRequiresTheCurrentPassword: a session left open on a shared
// machine must not be enough to take the account over.
func TestWebProfileRequiresTheCurrentPassword(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	_, page := h.webGet(t, c, "/web/profile")
	csrf := betweenMarkers(page, `name="csrf" value="`, `"`)

	resp, err := c.PostForm(h.http.URL+"/web/profile/password", url.Values{
		"csrf": {csrf}, "current": {"not-the-password"},
		"password": {"a-much-better-password"}, "confirm": {"a-much-better-password"},
	})
	if err != nil {
		t.Fatalf("change password: %v", err)
	}
	if body := readBody(t, resp); !strings.Contains(body, "not your current password") {
		t.Errorf("the wrong current password was accepted:\n%s", body)
	}
	// The original still works.
	ok := h.do("PROPFIND", "/remote.php/dav/files/alice/", "alice", alicePassword, "",
		map[string]string{"Depth": "0"})
	ok.Body.Close()
	if ok.StatusCode != http.StatusMultiStatus {
		t.Error("the password was changed anyway")
	}
}

// TestWebProfileAddsADevice is the reason this lives outside the admin page: an
// administrator generating the code has to then show it to somebody else.
func TestWebProfileAddsADevice(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	_, page := h.webGet(t, c, "/web/profile")
	csrf := betweenMarkers(page, `name="csrf" value="`, `"`)

	resp, err := c.PostForm(h.http.URL+"/web/profile/device",
		url.Values{"csrf": {csrf}, "label": {"Ana's phone"}})
	if err != nil {
		t.Fatalf("add device: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "<svg") {
		t.Errorf("no sign-in code rendered:\n%s", body)
	}
	if !strings.Contains(body, "nc://login/server:") {
		t.Errorf("the code does not carry a handoff URL:\n%s", body)
	}

	// The credential works, and appears in the device list.
	passwords, err := h.db.ListAppPasswords(t.Context(), aliceID(t, h))
	if err != nil || len(passwords) != 1 {
		t.Fatalf("expected one device credential, got %v (%v)", passwords, err)
	}
	if _, after := h.webGet(t, c, "/web/profile"); !strings.Contains(after, "Ana&#39;s phone") {
		t.Errorf("the device is not listed:\n%s", after)
	}
}

func bobUserID(t *testing.T, h *harness) int64 {
	t.Helper()
	u, err := h.db.UserByName(t.Context(), "bob")
	if err != nil {
		t.Fatalf("look up bob: %v", err)
	}
	return u.ID
}
