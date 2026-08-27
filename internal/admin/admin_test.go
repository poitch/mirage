package admin

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/store"
)

const adminPassword = "admin-password-long-enough"

type fixture struct {
	t     *testing.T
	http  *httptest.Server
	db    *store.DB
	homes string
}

func newFixture(t *testing.T, configManagesUsers bool) *fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	homes := filepath.Join(base, "homes")
	for _, name := range []string{"alice", "bob"} {
		if err := os.MkdirAll(filepath.Join(homes, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	db, err := store.Open(ctx, filepath.Join(base, "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	t.Setenv(EnvUsername, "root")
	t.Setenv(EnvPassword, adminPassword)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	storage := fsx.NewManager(0o640, 0o750, nil)
	t.Cleanup(func() { storage.Close() })

	ad, err := New(db, storage, index.NewScanner(db, storage, log),
		auth.NewAuthenticator(db, log), log, "http://mirage.test", configManagesUsers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()
	ad.Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &fixture{t: t, http: ts, db: db, homes: homes}
}

// client returns an HTTP client with a cookie jar, so a session persists.
func (f *fixture) client() *http.Client {
	f.t.Helper()
	jar, err := newJar()
	if err != nil {
		f.t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// signIn logs in and returns a client plus the session's CSRF token.
func (f *fixture) signIn(t *testing.T) (*http.Client, string) {
	t.Helper()
	c := f.client()
	resp, err := c.PostForm(f.http.URL+"/admin/login",
		url.Values{"username": {"root"}, "password": {adminPassword}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: status = %d, want 303", resp.StatusCode)
	}
	return c, f.csrf(t, c)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (f *fixture) csrf(t *testing.T, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(f.http.URL + "/admin/users")
	if err != nil {
		t.Fatalf("fetch users page: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	m := csrfRe.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF token on the page; status %d", resp.StatusCode)
	}
	return string(m[1])
}

func (f *fixture) post(t *testing.T, c *http.Client, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(f.http.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// readBody consumes a response body, closing it.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func (f *fixture) get(t *testing.T, c *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := c.Get(f.http.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestDisabledWithoutPassword(t *testing.T) {
	t.Setenv(EnvPassword, "")
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	storage := fsx.NewManager(0o640, 0o750, nil)
	defer storage.Close()

	// No password means the page is not built at all, so it is never routed.
	_, err = New(db, storage, index.NewScanner(db, storage, log),
		auth.NewAuthenticator(db, log), log, "http://x", false)
	if err != ErrDisabled {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

// TestRejectsShortAdminPassword: this page can repoint any account at any
// directory, so a trivially guessable password is refused rather than warned about.
func TestRejectsShortAdminPassword(t *testing.T) {
	t.Setenv(EnvPassword, "short")
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	storage := fsx.NewManager(0o640, 0o750, nil)
	defer storage.Close()

	if _, err := New(db, storage, index.NewScanner(db, storage, log),
		auth.NewAuthenticator(db, log), log, "http://x", false); err == nil {
		t.Fatal("a short admin password was accepted")
	}
}

func TestRequiresSignIn(t *testing.T) {
	f := newFixture(t, false)
	c := f.client()
	for _, path := range []string{"/admin/users", "/admin/users/new", "/admin/users/1"} {
		status, _ := f.get(t, c, path)
		if status != http.StatusSeeOther {
			t.Errorf("%s without a session: status = %d, want a redirect to login", path, status)
		}
	}
}

func TestRejectsWrongAdminCredentials(t *testing.T) {
	f := newFixture(t, false)
	c := f.client()
	for _, creds := range [][2]string{{"root", "wrong"}, {"wrong", adminPassword}, {"", ""}} {
		resp := f.post(t, c, "/admin/login", url.Values{"username": {creds[0]}, "password": {creds[1]}})
		resp.Body.Close()
		if resp.StatusCode == http.StatusSeeOther {
			t.Errorf("credentials %v were accepted", creds)
		}
	}
}

func TestCreateAndEditAccount(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)

	resp := f.post(t, c, "/admin/users", url.Values{
		"csrf": {csrf}, "username": {"alice"}, "display_name": {"Alice"},
		"home": {filepath.Join(f.homes, "alice")}, "uid": {"1026"}, "gid": {"100"},
		"quota_gb": {"10"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: status = %d, want 303", resp.StatusCode)
	}

	u, err := f.db.UserByName(context.Background(), "alice")
	if err != nil {
		t.Fatalf("account not created: %v", err)
	}
	if u.UID != 1026 || u.GID != 100 {
		t.Errorf("uid/gid = %d/%d, want 1026/100", u.UID, u.GID)
	}
	if u.Quota != 10*1024*1024*1024 {
		t.Errorf("quota = %d, want 10 GiB in bytes", u.Quota)
	}
	if u.PasswordHash != "" {
		t.Error("a new account should have no password until one is set")
	}

	// The listing should flag that it cannot be used yet.
	_, body := f.get(t, c, "/admin/users")
	if !strings.Contains(body, "no password") {
		t.Error("the listing does not flag an account with no password")
	}

	resp = f.post(t, c, "/admin/users/"+itoa(u.ID)+"/password",
		url.Values{"csrf": {csrf}, "password": {"alice-password"}})
	resp.Body.Close()
	u, _ = f.db.UserByName(context.Background(), "alice")
	if u.PasswordHash == "" {
		t.Error("password was not set")
	}
	if !auth.VerifyPassword(u.PasswordHash, "alice-password") {
		t.Error("the stored password does not verify")
	}
}

// TestCannotCreateOverlappingAccounts is the isolation invariant, enforced on
// the admin page as well as in the config file. The nested cases are the ones
// nothing downstream can catch.
func TestCannotCreateOverlappingAccounts(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)

	create := func(username, home string) int {
		resp := f.post(t, c, "/admin/users", url.Values{
			"csrf": {csrf}, "username": {username}, "home": {home},
			"uid": {"1026"}, "gid": {"100"},
		})
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := create("alice", filepath.Join(f.homes, "alice")); got != http.StatusSeeOther {
		t.Fatalf("first account: status = %d, want 303", got)
	}
	for _, tc := range []struct{ name, username, home string }{
		{"duplicate username", "alice", filepath.Join(f.homes, "bob")},
		{"the same directory", "carol", filepath.Join(f.homes, "alice")},
		{"inside another account", "carol", filepath.Join(f.homes, "alice", "shared")},
		{"containing another account", "carol", f.homes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := create(tc.username, tc.home); got == http.StatusSeeOther {
				t.Errorf("%s was accepted", tc.name)
			}
		})
	}
}

// TestCSRFRequired: the session cookie is SameSite=Lax, and the token is the
// belt to that braces.
func TestCSRFRequired(t *testing.T) {
	f := newFixture(t, false)
	c, _ := f.signIn(t)

	resp := f.post(t, c, "/admin/users", url.Values{
		"csrf": {"wrong-token"}, "username": {"mallory"},
		"home": {filepath.Join(f.homes, "bob")}, "uid": {"1026"}, "gid": {"100"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if _, err := f.db.UserByName(context.Background(), "mallory"); err == nil {
		t.Fatal("an account was created without a valid form token")
	}
}

// TestReadOnlyWhenConfigManagesUsers: two sources of truth for the account list
// would be worse than one, so the config file wins when it declares them.
func TestReadOnlyWhenConfigManagesUsers(t *testing.T) {
	f := newFixture(t, true)
	c, csrf := f.signIn(t)

	resp := f.post(t, c, "/admin/users", url.Values{
		"csrf": {csrf}, "username": {"alice"},
		"home": {filepath.Join(f.homes, "alice")}, "uid": {"1026"}, "gid": {"100"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}

	_, body := f.get(t, c, "/admin/users")
	if !strings.Contains(body, "config file") {
		t.Error("the page does not explain why it is read-only")
	}
	if strings.Contains(body, `href="/admin/users/new"`) {
		t.Error("the add button is shown while the page is read-only")
	}
}

func TestDeleteRequiresTypedConfirmation(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)

	f.post(t, c, "/admin/users", url.Values{
		"csrf": {csrf}, "username": {"alice"},
		"home": {filepath.Join(f.homes, "alice")}, "uid": {"1026"}, "gid": {"100"},
	}).Body.Close()
	u, _ := f.db.UserByName(context.Background(), "alice")

	// The wrong confirmation must not delete.
	f.post(t, c, "/admin/users/"+itoa(u.ID)+"/delete",
		url.Values{"csrf": {csrf}, "confirm": {"not-the-name"}}).Body.Close()
	if _, err := f.db.UserByName(context.Background(), "alice"); err != nil {
		t.Fatal("the account was deleted despite a wrong confirmation")
	}

	f.post(t, c, "/admin/users/"+itoa(u.ID)+"/delete",
		url.Values{"csrf": {csrf}, "confirm": {"alice"}}).Body.Close()
	if _, err := f.db.UserByName(context.Background(), "alice"); err == nil {
		t.Error("the account was not deleted")
	}
	// The files are not Mirage's to remove.
	if _, err := os.Stat(filepath.Join(f.homes, "alice")); err != nil {
		t.Errorf("deleting an account removed its files: %v", err)
	}
}

// TestStorageProblemIsSurfaced: a wrong directory is the most likely mistake,
// and it otherwise shows up as a confusing permission error mid-sync.
func TestStorageProblemIsSurfaced(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)

	f.post(t, c, "/admin/users", url.Values{
		"csrf": {csrf}, "username": {"ghost"},
		"home": {filepath.Join(f.homes, "does-not-exist")}, "uid": {"1026"}, "gid": {"100"},
	}).Body.Close()

	_, body := f.get(t, c, "/admin/users")
	if !strings.Contains(body, "does not exist") {
		t.Errorf("the page does not report the missing directory; got:\n%s", body)
	}
}

func TestSignOutEndsTheSession(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)

	f.post(t, c, "/admin/logout", url.Values{"csrf": {csrf}}).Body.Close()
	if status, _ := f.get(t, c, "/admin/users"); status != http.StatusSeeOther {
		t.Errorf("still signed in after logout: status = %d", status)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestDeviceSetupCodeSignsIn covers the point of the code: what it contains has
// to actually authenticate, or somebody scans it and gets a login screen.
func TestDeviceSetupCodeSignsIn(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)

	f.post(t, c, "/admin/users", url.Values{
		"csrf": {csrf}, "username": {"ana"},
		"home": {filepath.Join(f.homes, "alice")}, "uid": {"1026"}, "gid": {"100"},
	}).Body.Close()
	u, err := f.db.UserByName(context.Background(), "ana")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	resp := f.post(t, c, "/admin/users/"+itoa(u.ID)+"/device",
		url.Values{"csrf": {csrf}, "label": {"Ana's iPhone"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := html.UnescapeString(readBody(t, resp))

	// Rendered as markup rather than fetched, since the page forbids loading
	// anything at all.
	if !strings.Contains(body, "<svg ") {
		t.Error("no inline QR code on the page")
	}
	m := regexp.MustCompile(`nc://login/server:(\S+?)&user:(\S+?)&password:([A-Za-z0-9]+)`).
		FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no sign-in URL on the page:\n%s", body)
	}
	server, username, password := m[1], m[2], m[3]
	if server != "http://mirage.test" {
		t.Errorf("server = %q, want the configured external URL", server)
	}
	if username != "ana" {
		t.Errorf("user = %q, want ana", username)
	}

	// The credential in the code must work, and belong to that account alone.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authenticator := auth.NewAuthenticator(f.db, log)
	got, err := authenticator.Verify(context.Background(), "ana", password)
	if err != nil {
		t.Fatalf("the credential in the sign-in code does not authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("credential authenticated as %q", got.Username)
	}

	// It is a separate credential, named so it can be recognised and revoked.
	tokens, err := f.db.ListAppPasswords(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("ListAppPasswords: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Name != "Ana's iPhone" {
		t.Errorf("app passwords = %+v, want one named for the device", tokens)
	}
}

// TestDeviceSetupCodeIsNotCached: the page carries a working credential, so it
// must not sit in a browser cache or an intermediary.
func TestDeviceSetupCodeIsNotCached(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	f.post(t, c, "/admin/users", url.Values{
		"csrf": {csrf}, "username": {"ana"},
		"home": {filepath.Join(f.homes, "alice")}, "uid": {"1026"}, "gid": {"100"},
	}).Body.Close()
	u, _ := f.db.UserByName(context.Background(), "ana")

	resp := f.post(t, c, "/admin/users/"+itoa(u.ID)+"/device", url.Values{"csrf": {csrf}})
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

// TestDeviceSetupRequiresCSRF: minting a credential is exactly the request that
// must not be triggerable from another site.
func TestDeviceSetupRequiresCSRF(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	f.post(t, c, "/admin/users", url.Values{
		"csrf": {csrf}, "username": {"ana"},
		"home": {filepath.Join(f.homes, "alice")}, "uid": {"1026"}, "gid": {"100"},
	}).Body.Close()
	u, _ := f.db.UserByName(context.Background(), "ana")

	resp := f.post(t, c, "/admin/users/"+itoa(u.ID)+"/device", url.Values{"csrf": {"wrong"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	tokens, _ := f.db.ListAppPasswords(context.Background(), u.ID)
	if len(tokens) != 0 {
		t.Error("a credential was minted without a valid form token")
	}
}
