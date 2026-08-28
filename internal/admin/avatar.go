package admin

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/poitch/mirage/internal/avatar"
	"github.com/poitch/mirage/internal/store"
)

// avatarPreviewSize is what the admin page shows. Large enough to see the
// picture, small enough not to matter.
const avatarPreviewSize = 128

// showAvatar renders an account's picture for the admin page.
//
// The sync endpoint cannot be used here: it authenticates as a sync account,
// and the admin page has a session rather than one of those. This serves the
// same image behind the admin session instead.
func (ad *Admin) showAvatar(w http.ResponseWriter, r *http.Request, sess *session) {
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}

	png, err := ad.avatarPNG(r, u)
	if err != nil {
		ad.internalError(w, "render avatar", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Not cached: the page is where a picture is changed, and a stale preview
	// after an upload would look like the upload had failed.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func (ad *Admin) avatarPNG(r *http.Request, u store.User) ([]byte, error) {
	stored, err := ad.db.AvatarFor(r.Context(), u.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		if rendered, err := avatar.Render(stored.Image, avatarPreviewSize); err == nil {
			return rendered, nil
		}
	}
	return avatar.Generate(avatarSeed(u), avatarPreviewSize)
}

// setAvatar stores or removes an account's picture.
func (ad *Admin) setAvatar(w http.ResponseWriter, r *http.Request, sess *session) {
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}

	if r.PostFormValue("action") == "clear" {
		if err := ad.db.ClearAvatar(r.Context(), u.ID); err != nil {
			ad.internalError(w, "clear avatar", err)
			return
		}
		ad.log.Info("account picture removed from the admin page", "user", u.Username)
		ad.redirectWithNotice(w, r, u.ID, "Picture removed.")
		return
	}

	file, header, err := r.FormFile("avatar")
	if err != nil {
		ad.redirectWithNotice(w, r, u.ID, "Choose an image first.")
		return
	}
	defer file.Close()

	// Bounded here as well as by the body limit, so that a lie in the multipart
	// header cannot get past it.
	data, err := io.ReadAll(io.LimitReader(file, avatar.MaxUploadBytes+1))
	if err != nil {
		ad.internalError(w, "read uploaded image", err)
		return
	}

	normalised, err := avatar.Normalise(data)
	if err != nil {
		ad.log.Info("rejected an account picture",
			"user", u.Username, "filename", header.Filename, "reason", err)
		ad.redirectWithNotice(w, r, u.ID, err.Error())
		return
	}
	if err := ad.db.SetAvatar(r.Context(), u.ID, normalised); err != nil {
		ad.internalError(w, "store avatar", err)
		return
	}

	ad.log.Info("account picture set from the admin page",
		"user", u.Username, "bytes", len(normalised))
	ad.redirectWithNotice(w, r, u.ID, "Picture updated. Clients may take a day to notice.")
}

// avatarSeed matches what the sync endpoint uses, so the generated fallback
// shown here is the one clients are actually given.
func avatarSeed(u store.User) string {
	return strconv.FormatInt(u.ID, 10) + ":" + u.Username
}
