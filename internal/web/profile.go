package web

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/qrcode"
	"github.com/poitch/mirage/internal/store"
)

// minPasswordLen is the shortest password accepted, matching the CLI.
const minPasswordLen = 8

// device is one paired client, as shown on the profile page.
type device struct {
	ID       int64
	Label    string
	Created  string
	LastUsed string
}

// profile shows the account's own settings.
//
// Setting up a device lives here as well as on the admin page, because the
// admin version requires an administrator to be in the room: they generate the
// code and then have to show somebody else's screen to them. A person adding
// their own phone should not need anybody.
func (s *Site) profile(w http.ResponseWriter, r *http.Request, sess *session) {
	s.renderProfile(w, r, sess, pageData{})
}

// renderProfile draws the profile page, carrying over whatever the caller wants
// to say on it.
func (s *Site) renderProfile(w http.ResponseWriter, r *http.Request, sess *session, data pageData) {
	user, err := s.db.UserByID(r.Context(), sess.userID)
	if err != nil {
		s.internalError(w, "look up account", err)
		return
	}
	l := languageFor(r)
	data.Lang = l
	data.Title = user.Username
	data.Username = sess.username
	data.CSRF = sess.csrf
	data.DisplayName = user.DisplayName
	if data.DisplayName == "" {
		data.DisplayName = user.Username
	}
	if data.Notice == "" {
		data.Notice = r.URL.Query().Get("notice")
	}

	passwords, err := s.db.ListAppPasswords(r.Context(), user.ID)
	if err != nil {
		// The page is still worth showing without the list.
		s.log.Warn("could not list devices", "user", user.Username, "error", err)
	}
	for _, p := range passwords {
		d := device{ID: p.ID, Label: p.Name, Created: l.DateOnly(p.CreatedAt)}
		if d.Label == "" {
			d.Label = data.T("profile.unnamed")
		}
		if !p.LastUsedAt.IsZero() {
			d.LastUsed = l.DateOnly(p.LastUsedAt)
		}
		data.Devices = append(data.Devices, d)
	}

	status := http.StatusOK
	if data.Error != "" {
		status = http.StatusBadRequest
	}
	s.render(w, r, "profile.html", status, data)
}

// changePassword sets a new account password.
func (s *Site) changePassword(w http.ResponseWriter, r *http.Request, sess *session) {
	user, err := s.db.UserByID(r.Context(), sess.userID)
	if err != nil {
		s.internalError(w, "look up account", err)
		return
	}

	current := r.PostFormValue("current")
	next := r.PostFormValue("password")
	confirm := r.PostFormValue("confirm")

	// The current password is required even though the session is already
	// authenticated: a session left open on a shared machine should not be
	// enough to take the account over.
	if _, err := s.auth.Verify(r.Context(), user.Username, current); err != nil {
		s.log.Info("rejected a password change with the wrong current password",
			"user", user.Username)
		s.renderProfile(w, r, sess, pageData{Error: s.t(r, "profile.notCurrent")})
		return
	}
	switch {
	case next != confirm:
		s.renderProfile(w, r, sess, pageData{Error: s.t(r, "profile.mismatch")})
		return
	case len(next) < minPasswordLen:
		s.renderProfile(w, r, sess, pageData{
			Error: s.tf(r, "profile.tooShort", minPasswordLen),
		})
		return
	case next == current:
		s.renderProfile(w, r, sess, pageData{Error: s.t(r, "profile.same")})
		return
	}

	hash, err := auth.HashPassword(next)
	if err != nil {
		s.internalError(w, "hash password", err)
		return
	}
	if err := s.db.SetPasswordHash(r.Context(), user.ID, hash); err != nil {
		s.internalError(w, "store password", err)
		return
	}
	// The cached credential has to stop working at once, or the old password
	// would keep opening the account until the cache expired.
	s.auth.Forget(user.Username)

	s.log.Info("account password changed by its owner", "user", user.Username)
	// Deliberately not signing other sessions out: paired devices have their
	// own credentials and are unaffected, and being logged out of every browser
	// for changing a password is a surprise rather than a safeguard.
	s.renderProfile(w, r, sess, pageData{
		Notice: s.t(r, "profile.changed"),
	})
}

// addDevice issues a one-time sign-in code for a new client.
func (s *Site) addDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	user, err := s.db.UserByID(r.Context(), sess.userID)
	if err != nil {
		s.internalError(w, "look up account", err)
		return
	}

	appPassword, err := auth.GenerateAppPassword()
	if err != nil {
		s.internalError(w, "generate device credential", err)
		return
	}
	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" {
		label = "Added from the web"
	}
	if _, err := s.db.CreateAppPassword(r.Context(), user.ID, label, auth.HashToken(appPassword)); err != nil {
		s.internalError(w, "store device credential", err)
		return
	}

	setupURL := auth.HandoffURL(s.externalURL, user.Username, appPassword)
	code, err := qrcode.SVG(setupURL)
	if err != nil {
		s.internalError(w, "render sign-in code", err)
		return
	}

	s.log.Info("issued a device sign-in code from the web", "user", user.Username, "label", label)
	// Held only for this render. The credential cannot be recovered afterwards,
	// which is the point: it exists on the device and nowhere else.
	s.renderProfile(w, r, sess, pageData{
		QR: code, SetupURL: template.URL(setupURL),
	})
}

// revokeDevice withdraws one device's credential.
func (s *Site) revokeDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		s.renderProfile(w, r, sess, pageData{Error: s.t(r, "profile.noDevice")})
		return
	}
	// Scoped to the account, so an id belonging to somebody else revokes
	// nothing and reports the same thing as one that never existed.
	err = s.db.DeleteAppPasswordByID(r.Context(), sess.userID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderProfile(w, r, sess, pageData{Error: s.t(r, "profile.noDevice")})
		return
	} else if err != nil {
		s.internalError(w, "revoke device", err)
		return
	}
	// Cached credentials must stop working now rather than when they expire.
	s.auth.Forget(sess.username)

	s.log.Info("device credential revoked by its owner", "user", sess.username, "device", id)
	s.renderProfile(w, r, sess, pageData{Notice: s.t(r, "profile.revoked")})
}
