package admin

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/poitch/mirage/internal/store"
)

// makeAccount creates an account through the page, which is how one is made in
// practice.
func makeAccount(t *testing.T, f *fixture, c *http.Client, csrf, name string) store.User {
	t.Helper()
	resp := f.post(t, c, "/admin/users", map[string][]string{
		"csrf": {csrf}, "username": {name}, "display_name": {name},
		"home": {f.homes + "/" + name}, "uid": {"1026"}, "gid": {"100"},
	})
	resp.Body.Close()
	u, err := f.db.UserByName(context.Background(), name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return u
}

func pngBytes(t *testing.T, size int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// uploadAvatar posts a picture as the form does.
func uploadAvatar(t *testing.T, f *fixture, c *http.Client, csrf string, id int64,
	filename string, content []byte) *http.Response {

	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("csrf", csrf); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := mw.CreateFormFile("avatar", filename)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req, err := http.NewRequest("POST",
		f.http.URL+"/admin/users/"+itoa(id)+"/avatar", &body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return resp
}

func TestAdminUploadsAndRemovesAnAvatar(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	u := makeAccount(t, f, c, csrf, "alice")

	if _, err := f.db.AvatarFor(context.Background(), u.ID); err == nil {
		t.Fatal("a new account already had a picture")
	}

	resp := uploadAvatar(t, f, c, csrf, u.ID, "face.png", pngBytes(t, 300, color.RGBA{G: 0xff, A: 0xff}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload: status = %d, want 303", resp.StatusCode)
	}
	stored, err := f.db.AvatarFor(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("picture was not stored: %v", err)
	}
	// Stored normalised, not as sent, so that serving a size is cheap and one
	// enormous upload cannot bloat the database.
	img, err := png.Decode(bytes.NewReader(stored.Image))
	if err != nil {
		t.Fatalf("stored bytes are not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != b.Dy() {
		t.Errorf("stored picture is %dx%d, not square", b.Dx(), b.Dy())
	}

	clear := f.post(t, c, "/admin/users/"+itoa(u.ID)+"/avatar",
		map[string][]string{"csrf": {csrf}, "action": {"clear"}})
	clear.Body.Close()
	if _, err := f.db.AvatarFor(context.Background(), u.ID); err == nil {
		t.Error("the picture survived being removed")
	}
}

func TestAdminPreviewsTheAvatar(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	u := makeAccount(t, f, c, csrf, "alice")

	// Before any upload the preview is the generated mark, so the page always
	// shows something.
	resp, err := c.Get(f.http.URL + "/admin/users/" + itoa(u.ID) + "/avatar")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview: status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("preview content type = %q, want image/png", ct)
	}
	if _, err := png.Decode(strings.NewReader(body)); err != nil {
		t.Errorf("preview is not a PNG: %v", err)
	}
}

// TestAvatarPreviewNeedsASession: it is behind the admin session like every
// other page, and an unauthenticated fetch must not reach it.
func TestAvatarPreviewNeedsASession(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	u := makeAccount(t, f, c, csrf, "alice")

	anon := f.client()
	resp, err := anon.Get(f.http.URL + "/admin/users/" + itoa(u.ID) + "/avatar")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect to the login page", resp.StatusCode)
	}
}

// TestAvatarUploadRejectsWhatIsNotAPicture: the message goes back on the page,
// so somebody who picked the wrong file is told what happened.
func TestAvatarUploadRejectsWhatIsNotAPicture(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	u := makeAccount(t, f, c, csrf, "alice")

	resp := uploadAvatar(t, f, c, csrf, u.ID, "notes.txt", []byte("this is not a picture"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 back to the page", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "notice=") {
		t.Errorf("the rejection said nothing: Location = %q", loc)
	}
	if _, err := f.db.AvatarFor(context.Background(), u.ID); err == nil {
		t.Error("a file that is not a picture was stored anyway")
	}
}

// TestAvatarUploadNeedsTheCSRFToken: it is a write like any other.
func TestAvatarUploadNeedsTheCSRFToken(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	u := makeAccount(t, f, c, csrf, "alice")

	resp := uploadAvatar(t, f, c, "wrong-token", u.ID, "face.png",
		pngBytes(t, 64, color.RGBA{R: 0xff, A: 0xff}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if _, err := f.db.AvatarFor(context.Background(), u.ID); err == nil {
		t.Error("a request with a bad token stored a picture")
	}
}

// TestTheFormOffersTheUpload guards the wiring between the handler and the
// page, which is otherwise only exercised by a person looking at it.
func TestTheFormOffersTheUpload(t *testing.T) {
	f := newFixture(t, false)
	c, csrf := f.signIn(t)
	u := makeAccount(t, f, c, csrf, "alice")

	status, body := f.get(t, c, "/admin/users/"+itoa(u.ID))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{
		`enctype="multipart/form-data"`,
		`name="avatar"`,
		"/admin/users/" + itoa(u.ID) + "/avatar",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the account page does not offer the upload: missing %s", want)
		}
	}
	// The remove button only makes sense once there is something to remove.
	if strings.Contains(body, `value="clear"`) {
		t.Error("the page offered to remove a picture that does not exist")
	}

	up := uploadAvatar(t, f, c, csrf, u.ID, "face.png", pngBytes(t, 128, color.RGBA{B: 0xff, A: 0xff}))
	up.Body.Close()
	if _, body := f.get(t, c, "/admin/users/"+itoa(u.ID)); !strings.Contains(body, `value="clear"`) {
		t.Error("the page did not offer to remove an uploaded picture")
	}
}
