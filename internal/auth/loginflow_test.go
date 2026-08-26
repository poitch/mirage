package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testLoginFlow(t *testing.T) (*LoginFlow, http.Handler) {
	t.Helper()
	a, db, _ := testAuth(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	lf, err := NewLoginFlow(db, a, "https://mirage.example.com/", log)
	if err != nil {
		t.Fatalf("NewLoginFlow: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /index.php/login/v2", lf.Start)
	mux.HandleFunc("POST /login/v2/poll", lf.Poll)
	mux.HandleFunc("GET /index.php/login/v2/flow/{token}", lf.Page)
	mux.HandleFunc("POST /index.php/login/v2/flow/{token}", lf.Page)
	return lf, mux
}

// start opens a pairing session and returns the poll token and the login path.
func start(t *testing.T, h http.Handler, userAgent string) (pollToken, loginPath string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/index.php/login/v2", nil)
	req.Header.Set("User-Agent", userAgent)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("start: status = %d, want 200", rec.Code)
	}
	var got startResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("start: decode: %v", err)
	}
	u, err := url.Parse(got.Login)
	if err != nil {
		t.Fatalf("start: login URL: %v", err)
	}
	return got.Poll.Token, u.Path
}

func poll(t *testing.T, h http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login/v2/poll",
		strings.NewReader("token="+url.QueryEscape(token)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func approve(t *testing.T, h http.Handler, loginPath, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginFlowHappyPath(t *testing.T) {
	lf, h := testLoginFlow(t)
	pollToken, loginPath := start(t, h, "Mozilla/5.0 (Macintosh) mirall/3.13.0")

	// Nothing is granted until the user signs in, and the client is told so
	// with a 404 rather than an error it would give up on.
	if rec := poll(t, h, pollToken); rec.Code != http.StatusNotFound {
		t.Fatalf("poll before approval: status = %d, want 404", rec.Code)
	}

	// The approval page names the device, which is the only thing telling the
	// user what they are about to authorise.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, loginPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login page: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nextcloud desktop 3.13.0 on macOS") {
		t.Error("login page does not name the requesting device")
	}

	if rec := approve(t, h, loginPath, "alice", "alice-account-password"); rec.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, want 200", rec.Code)
	}

	rec = poll(t, h, pollToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll after approval: status = %d, want 200", rec.Code)
	}
	var granted pollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &granted); err != nil {
		t.Fatalf("decode poll response: %v", err)
	}
	if granted.LoginName != "alice" {
		t.Errorf("loginName = %q, want alice", granted.LoginName)
	}
	// The trailing slash in the configured external URL must not survive into
	// what the client stores as its server address.
	if granted.Server != "https://mirage.example.com" {
		t.Errorf("server = %q, want no trailing slash", granted.Server)
	}
	if len(granted.AppPassword) != appPasswordLen {
		t.Fatalf("app password length = %d, want %d", len(granted.AppPassword), appPasswordLen)
	}

	// The token must actually work.
	user, err := lf.auth.Verify(context.Background(), "alice", granted.AppPassword)
	if err != nil {
		t.Fatalf("granted app password does not authenticate: %v", err)
	}
	if user.Username != "alice" {
		t.Errorf("token belongs to %q, want alice", user.Username)
	}
}

// TestLoginFlowDeliversOnce guards against a replayed or intercepted poll token
// being redeemed a second time.
func TestLoginFlowDeliversOnce(t *testing.T) {
	_, h := testLoginFlow(t)
	pollToken, loginPath := start(t, h, "mirall/3.13.0")
	approve(t, h, loginPath, "alice", "alice-account-password")

	if rec := poll(t, h, pollToken); rec.Code != http.StatusOK {
		t.Fatalf("first poll: status = %d, want 200", rec.Code)
	}
	if rec := poll(t, h, pollToken); rec.Code != http.StatusNotFound {
		t.Fatalf("replayed poll: status = %d, want 404", rec.Code)
	}
}

// TestLoginFlowGrantsOnlyOnce stops a single pairing link from minting an
// unbounded number of device tokens.
func TestLoginFlowGrantsOnlyOnce(t *testing.T) {
	_, h := testLoginFlow(t)
	_, loginPath := start(t, h, "mirall/3.13.0")

	if rec := approve(t, h, loginPath, "alice", "alice-account-password"); rec.Code != http.StatusOK {
		t.Fatalf("first approval: status = %d, want 200", rec.Code)
	}
	rec := approve(t, h, loginPath, "alice", "alice-account-password")
	if rec.Code != http.StatusNotFound {
		t.Errorf("second approval: status = %d, want 404", rec.Code)
	}
}

func TestLoginFlowRejectsBadCredentials(t *testing.T) {
	_, h := testLoginFlow(t)
	pollToken, loginPath := start(t, h, "mirall/3.13.0")

	if rec := approve(t, h, loginPath, "alice", "wrong-password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: status = %d, want 401", rec.Code)
	}
	// A failed sign-in must not have granted anything.
	if rec := poll(t, h, pollToken); rec.Code != http.StatusNotFound {
		t.Errorf("poll after failed sign-in: status = %d, want 404", rec.Code)
	}
}

func TestLoginFlowUnknownToken(t *testing.T) {
	_, h := testLoginFlow(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.php/login/v2/flow/NOSUCHTOKEN", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown login token: status = %d, want 404", rec.Code)
	}
	if rec := poll(t, h, "NOSUCHTOKEN"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown poll token: status = %d, want 404", rec.Code)
	}
}

// TestPairingPagesAreNotFrameable: the page takes a password, so it must not be
// embeddable in someone else's site.
func TestPairingPagesAreNotFrameable(t *testing.T) {
	_, h := testLoginFlow(t)
	_, loginPath := start(t, h, "mirall/3.13.0")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, loginPath, nil))
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q, want frame-ancestors 'none'", csp)
	}
}

func TestDescribeClient(t *testing.T) {
	tests := []struct{ agent, want string }{
		{"Mozilla/5.0 (Macintosh) mirall/3.13.0", "Nextcloud desktop 3.13.0 on macOS"},
		{"Mozilla/5.0 (Windows NT 10.0) mirall/3.15.3", "Nextcloud desktop 3.15.3 on Windows"},
		{"Nextcloud-android/3.29.0", "Nextcloud-android 3.29.0 on Android"},
		{"Mozilla/5.0 (iPhone) Nextcloud-iOS/4.9.0", "Nextcloud-iOS 4.9.0 on iOS"},
		{"", "Unknown device"},
		{"curl/8.7.1", "curl/8.7.1"},
	}
	for _, tc := range tests {
		if got := describeClient(tc.agent); got != tc.want {
			t.Errorf("describeClient(%q) = %q, want %q", tc.agent, got, tc.want)
		}
	}
}

// TestDescribeClientIsBounded keeps a hostile User-Agent from filling the
// approval page, since the value is attacker-chosen and shown to the user.
func TestDescribeClientIsBounded(t *testing.T) {
	got := describeClient(strings.Repeat("A", 5000))
	if len(got) > 100 {
		t.Errorf("describeClient returned %d chars, want it truncated", len(got))
	}
}
