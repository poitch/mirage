package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/poitch/mirage/internal/avatar"
)

func TestAvatarIsServedOnEveryPathTheClientsUse(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/remote.php/dav/avatars/alice/128.png",
		"/index.php/avatar/alice/64",
		"/avatar/alice/64",
		"/index.php/avatar/alice/64/dark",
	} {
		resp := h.do("GET", path, "alice", alicePassword, "", nil)
		body := []byte(readBody(t, resp))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("GET %s content type = %q, want image/png", path, ct)
		}
		if _, err := png.Decode(bytes.NewReader(body)); err != nil {
			t.Errorf("GET %s did not return a PNG: %v", path, err)
		}
	}
}

func TestAvatarSizeComesFromThePath(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/remote.php/dav/avatars/alice/64.png", 64},
		{"/remote.php/dav/avatars/alice/512.png", 512},
		// Unreadable sizes fall back rather than failing: the client wants a
		// picture, and there is a reasonable one to give it.
		{"/remote.php/dav/avatars/alice/huge.png", 128},
	} {
		resp := h.do("GET", tc.path, "alice", alicePassword, "", nil)
		img, err := png.Decode(strings.NewReader(readBody(t, resp)))
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		if got := img.Bounds().Dx(); got != tc.want {
			t.Errorf("GET %s returned %dpx, want %d", tc.path, got, tc.want)
		}
	}
}

// TestAvatarIsCacheable: the desktop client requests avatars constantly, and
// answering every one with a fresh image is waste the ETag exists to avoid.
func TestAvatarIsCacheable(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/remote.php/dav/avatars/alice/128.png", "alice", alicePassword, "", nil)
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if etag == "" {
		t.Fatal("no ETag on the avatar response")
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want a max-age", cc)
	}

	again := h.do("GET", "/remote.php/dav/avatars/alice/128.png", "alice", alicePassword, "",
		map[string]string{"If-None-Match": etag})
	body := readBody(t, again)
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("conditional request = %d, want 304", again.StatusCode)
	}
	if body != "" {
		t.Errorf("304 carried a body of %d bytes", len(body))
	}
}

// TestAvatarMarksItselfGenerated tells the client the picture is not one the
// person chose, which is what decides whether it offers an upload control.
func TestAvatarMarksItselfGenerated(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/remote.php/dav/avatars/alice/128.png", "alice", alicePassword, "", nil)
	defer resp.Body.Close()
	if got := resp.Header.Get("X-NC-IsCustomAvatar"); got != "0" {
		t.Errorf("X-NC-IsCustomAvatar = %q, want 0", got)
	}
}

func TestAvatarRequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/remote.php/dav/avatars/alice/128.png", "", "", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated avatar = %d, want 401", resp.StatusCode)
	}
}

// TestAvatarOfAnotherAccountIsAllowed: avatars are not private, and clients
// fetch other people's when rendering shares. The account list is not a secret
// among family members, but an unknown name still 404s.
func TestAvatarOfAnotherAccountIsAllowed(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/remote.php/dav/avatars/bob/128.png", "alice", alicePassword, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("alice fetching bob's avatar = %d, want 200", resp.StatusCode)
	}

	missing := h.do("GET", "/remote.php/dav/avatars/nobody/128.png", "alice", alicePassword, "", nil)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("avatar for an unknown account = %d, want 404", missing.StatusCode)
	}
}

func TestAvatarsDifferBetweenAccounts(t *testing.T) {
	h := newHarness(t)
	a := readBody(t, h.do("GET", "/remote.php/dav/avatars/alice/128.png", "alice", alicePassword, "", nil))
	b := readBody(t, h.do("GET", "/remote.php/dav/avatars/bob/128.png", "alice", alicePassword, "", nil))
	if a == b {
		t.Error("alice and bob were given the same avatar")
	}
}

func TestProvisioningUserDetails(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/ocs/v2.php/cloud/users/alice?format=json", "alice", alicePassword, "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		OCS struct {
			Meta struct {
				StatusCode int `json:"statuscode"`
			} `json:"meta"`
			Data struct {
				ID          string `json:"id"`
				DisplayName string `json:"displayname"`
				Enabled     bool   `json:"enabled"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if env.OCS.Meta.StatusCode != 200 {
		t.Errorf("statuscode = %d, want 200", env.OCS.Meta.StatusCode)
	}
	if env.OCS.Data.ID != "alice" || env.OCS.Data.DisplayName != "Alice" || !env.OCS.Data.Enabled {
		t.Errorf("data = %+v, want alice/Alice/enabled", env.OCS.Data)
	}
}

// TestProvisioningRefusesOtherAccounts: this endpoint is the one place the API
// takes a username in the path and would otherwise happily describe somebody
// else's account.
func TestProvisioningRefusesOtherAccounts(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/ocs/v2.php/cloud/users/bob?format=json", "alice", alicePassword, "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if strings.Contains(body, "Bob") {
		t.Errorf("the refusal leaked bob's details: %s", body)
	}
}

// TestChattyEndpointsAnswer: each of these was retried on every connection
// while it went unhandled.
func TestChattyEndpointsAnswer(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/ocs/v2.php/core/navigation/apps?format=json",
		"/ocs/v2.php/apps/terms_of_service/terms?format=json",
		"/ocs/v2.php/apps/files/api/v1/directEditing?format=json",
		"/ocs/v2.php/apps/dashboard/api/v1/widgets?format=json",
	} {
		resp := h.do("GET", path, "alice", alicePassword, "", nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", path, resp.StatusCode, body)
		}
	}
}

// TestPushRegistrationIsRefused: Mirage has no notification relay, and a
// success here would leave the app waiting for pushes that never come.
func TestPushRegistrationIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/ocs/v2.php/apps/notifications/api/v2/push?format=json",
		"alice", alicePassword, "pushTokenHash=x&devicePublicKey=y&proxyServer=z",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// storeAvatar puts an uploaded picture on an account, as the admin page does.
func storeAvatar(t *testing.T, h *harness, username string, c color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	normalised, err := avatar.Normalise(buf.Bytes())
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	u, err := h.db.UserByName(t.Context(), username)
	if err != nil {
		t.Fatalf("look up %s: %v", username, err)
	}
	if err := h.db.SetAvatar(t.Context(), u.ID, normalised); err != nil {
		t.Fatalf("set avatar: %v", err)
	}
}

func TestUploadedAvatarIsServedInsteadOfTheGeneratedOne(t *testing.T) {
	h := newHarness(t)
	generated := readBody(t, h.do("GET", "/remote.php/dav/avatars/alice/128.png",
		"alice", alicePassword, "", nil))

	storeAvatar(t, h, "alice", color.RGBA{R: 0x20, G: 0xa0, B: 0x60, A: 0xff})

	resp := h.do("GET", "/remote.php/dav/avatars/alice/128.png", "alice", alicePassword, "", nil)
	body := readBody(t, resp)
	if body == generated {
		t.Fatal("the uploaded picture was ignored")
	}
	if got := resp.Header.Get("X-NC-IsCustomAvatar"); got != "1" {
		t.Errorf("X-NC-IsCustomAvatar = %q, want 1 for an uploaded picture", got)
	}
	img, err := png.Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := img.Bounds().Dx(); got != 128 {
		t.Errorf("uploaded picture served at %dpx, want 128", got)
	}
	r, g, b, _ := img.At(64, 64).RGBA()
	if r>>8 > 0x60 || g>>8 < 0x80 || b>>8 > 0x90 {
		t.Errorf("served picture is not the uploaded colour: %02x%02x%02x", r>>8, g>>8, b>>8)
	}
}

// TestChangingTheAvatarChangesTheETag: clients cache these for a day, so a new
// picture that reuses the old tag would not appear until the cache expired.
func TestChangingTheAvatarChangesTheETag(t *testing.T) {
	h := newHarness(t)
	etag := func() string {
		resp := h.do("GET", "/remote.php/dav/avatars/alice/128.png", "alice", alicePassword, "", nil)
		resp.Body.Close()
		return resp.Header.Get("ETag")
	}

	before := etag()
	storeAvatar(t, h, "alice", color.RGBA{R: 0xff, A: 0xff})
	afterUpload := etag()
	if afterUpload == before {
		t.Error("uploading a picture did not change the ETag")
	}

	storeAvatar(t, h, "alice", color.RGBA{B: 0xff, A: 0xff})
	if etag() == afterUpload {
		t.Error("replacing the picture did not change the ETag")
	}

	u, _ := h.db.UserByName(t.Context(), "alice")
	if err := h.db.ClearAvatar(t.Context(), u.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := etag(); got != before {
		t.Errorf("removing the picture gave ETag %q, want the generated one %q", got, before)
	}
}

func TestClearedAvatarFallsBackToTheGeneratedOne(t *testing.T) {
	h := newHarness(t)
	storeAvatar(t, h, "alice", color.RGBA{R: 0xff, A: 0xff})
	u, _ := h.db.UserByName(t.Context(), "alice")
	if err := h.db.ClearAvatar(t.Context(), u.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	resp := h.do("GET", "/remote.php/dav/avatars/alice/128.png", "alice", alicePassword, "", nil)
	defer resp.Body.Close()
	if got := resp.Header.Get("X-NC-IsCustomAvatar"); got != "0" {
		t.Errorf("X-NC-IsCustomAvatar = %q, want 0 after removing the picture", got)
	}
}
