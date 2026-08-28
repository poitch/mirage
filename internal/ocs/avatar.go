package ocs

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/avatar"
	"github.com/poitch/mirage/internal/store"
)

// avatarMaxAge is how long a client may keep an avatar without asking again.
// A generated avatar only changes if the account is deleted and remade, so a
// day is conservative.
const avatarMaxAge = 86400

// Avatar serves the small identity image the clients request for an account.
//
// Any signed-in account may fetch any other's: an avatar carries no private
// information, and clients request them for other people alongside shares. The
// endpoint is still behind authentication so it does not become a way to
// enumerate accounts from outside.
func (s *Service) Avatar(w http.ResponseWriter, r *http.Request) {
	_ = auth.MustUser(r.Context())

	username := r.PathValue("user")
	size := avatarSize(r.PathValue("size"))

	u, err := s.db.UserByName(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.log.Error("could not look up an account for its avatar", "user", username, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Seeded on the account id as well as the name, so that deleting an account
	// and making another with the same name does not silently inherit its mark.
	etag := avatar.ETag(avatarSeed(u), size)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(avatarMaxAge))
	// Clients read this to decide whether the picture is the person's own or one
	// the server made up; some will offer an upload control when it is 0.
	w.Header().Set("X-NC-IsCustomAvatar", "0")

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	png, err := avatar.Generate(avatarSeed(u), size)
	if err != nil {
		s.log.Error("could not draw an avatar", "user", username, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(png)
}

// avatarSeed is what the image is derived from.
func avatarSeed(u store.User) string {
	return strconv.FormatInt(u.ID, 10) + ":" + u.Username
}

// avatarSize reads the size out of the path, which clients write as "128" or
// "128.png". An unreadable one falls back to the default rather than failing:
// the client wants a picture.
func avatarSize(raw string) int {
	raw = strings.TrimSuffix(raw, ".png")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return avatar.DefaultSize
	}
	return n
}

// etagMatches reports whether an If-None-Match header covers etag.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
