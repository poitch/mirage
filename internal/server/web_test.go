package server

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
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
