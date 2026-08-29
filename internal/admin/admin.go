package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/avatar"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/qrcode"
	"github.com/poitch/mirage/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Environment variables holding the admin credentials.
const (
	EnvUsername = "MIRAGE_ADMIN_USERNAME"
	EnvPassword = "MIRAGE_ADMIN_PASSWORD"
)

// defaultUsername is used when only a password is supplied.
const defaultUsername = "admin"

// minAdminPassword is the shortest admin password accepted. This page can
// repoint any account at any directory, so a trivially guessable password on it
// is worth refusing outright rather than warning about.
const minAdminPassword = 12

// failedLoginDelay slows credential guessing. A floor, not a rate limiter.
const failedLoginDelay = 750 * time.Millisecond

// Admin serves the account management page.
type Admin struct {
	db       *store.DB
	storage  *fsx.Manager
	scanner  *index.Scanner
	auth     *auth.Authenticator
	log      *slog.Logger
	tmpl     *template.Template
	sessions *sessionStore
	// credentialsChanged is told when an account's password changes or it is
	// disabled, so that sessions held elsewhere can be ended.
	credentialsChanged func(userID int64)

	username    string
	password    string
	externalURL string
	secure      bool
	// readOnly is set when the config file declares the account list, in which
	// case it is authoritative and this page must not contradict it.
	readOnly bool
}

// ErrDisabled reports that no admin credentials were configured.
var ErrDisabled = errors.New("admin page is disabled")

// New builds the admin page from the environment.
//
// It returns ErrDisabled when no password is set, and the caller then does not
// route it at all. That is the deliberate default: an admin page that appears
// with a blank or guessable password would be worse than no admin page.
func New(db *store.DB, storage *fsx.Manager, scanner *index.Scanner, a *auth.Authenticator,
	log *slog.Logger, externalURL string, configManagesUsers bool) (*Admin, error) {

	password := os.Getenv(EnvPassword)
	if password == "" {
		return nil, ErrDisabled
	}
	if len(password) < minAdminPassword {
		return nil, fmt.Errorf("%s must be at least %d characters", EnvPassword, minAdminPassword)
	}
	username := os.Getenv(EnvUsername)
	if username == "" {
		username = defaultUsername
	}

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"bytes": humanBytes,
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	return &Admin{
		db: db, storage: storage, scanner: scanner, auth: a, log: log,
		tmpl: tmpl, sessions: newSessionStore(),
		username: username, password: password,
		externalURL: strings.TrimRight(externalURL, "/"),
		secure:      strings.HasPrefix(externalURL, "https://"),
		readOnly:    configManagesUsers,
	}, nil
}

// Routes registers the admin endpoints on mux.
func (ad *Admin) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /admin/login", ad.loginPage)
	mux.HandleFunc("POST /admin/login", ad.login)
	mux.HandleFunc("POST /admin/logout", ad.logout)

	mux.Handle("GET /admin/users", ad.guard(ad.listUsers))
	mux.Handle("GET /admin/users/new", ad.guard(ad.newUserForm))
	mux.Handle("POST /admin/users", ad.guard(ad.createUser))
	mux.Handle("GET /admin/users/{id}", ad.guard(ad.editUserForm))
	mux.Handle("POST /admin/users/{id}", ad.guard(ad.updateUser))
	mux.Handle("POST /admin/users/{id}/password", ad.guard(ad.setPassword))
	mux.Handle("POST /admin/users/{id}/state", ad.guard(ad.setState))
	mux.Handle("POST /admin/users/{id}/delete", ad.guard(ad.deleteUser))
	mux.Handle("POST /admin/users/{id}/scan", ad.guard(ad.scanUser))
	mux.Handle("POST /admin/users/{id}/device", ad.guard(ad.setUpDevice))
	mux.Handle("GET /admin/users/{id}/avatar", ad.guard(ad.showAvatar))
	mux.Handle("POST /admin/users/{id}/avatar", ad.guard(ad.setAvatar))
}

// maxAdminBody caps an admin form submission. Everything here is a handful of
// fields except the account picture, which sets the size.
const maxAdminBody = avatar.MaxUploadBytes + (1 << 20)

// OnCredentialsChanged registers a callback for when an account's password
// changes or it is disabled here.
//
// The browser view holds its own sessions, and a password changed on this page
// has to end them. It is a callback rather than a direct reference because the
// admin page has no business knowing what else exists.
func (ad *Admin) OnCredentialsChanged(f func(userID int64)) { ad.credentialsChanged = f }

// credentialsChangedFor runs the hook, if one is registered.
func (ad *Admin) credentialsChangedFor(userID int64) {
	if ad.credentialsChanged != nil {
		ad.credentialsChanged(userID)
	}
}

// guard requires a live session, and a matching CSRF token on writes.
func (ad *Admin) guard(h func(http.ResponseWriter, *http.Request, *session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			ad.redirectToLogin(w, r)
			return
		}
		sess, ok := ad.sessions.lookup(cookie.Value)
		if !ok {
			clearSessionCookie(w, ad.secure)
			ad.redirectToLogin(w, r)
			return
		}
		// Bounded before anything reads the form. One of these endpoints takes
		// an uploaded image, and r.FormValue would otherwise buffer whatever
		// arrived before the size could be checked.
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)
		}
		// The session cookie is SameSite=Lax, which stops cross-site POSTs on
		// its own; the token is the belt to that braces, and costs nothing.
		if r.Method == http.MethodPost && !sess.validCSRF(r.FormValue("csrf")) {
			ad.log.Warn("rejected an admin request with a bad CSRF token", "path", r.URL.Path)
			http.Error(w, "invalid form token; reload the page and try again", http.StatusForbidden)
			return
		}
		h(w, r, sess)
	})
}

func (ad *Admin) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (ad *Admin) loginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := ad.sessions.lookup(cookie.Value); ok {
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
			return
		}
	}
	ad.render(w, "login.html", http.StatusOK, pageData{Title: "Sign in"})
}

func (ad *Admin) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		ad.render(w, "login.html", http.StatusBadRequest, pageData{Title: "Sign in", Error: "That form could not be read."})
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	// Both compared in constant time, and both compared always, so the reply
	// takes the same path whether it was the name or the password that was
	// wrong.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(ad.username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(ad.password)) == 1
	if !userOK || !passOK {
		ad.log.Warn("rejected an admin sign-in", "username", username, "agent", r.UserAgent())
		time.Sleep(failedLoginDelay)
		ad.render(w, "login.html", http.StatusUnauthorized,
			pageData{Title: "Sign in", Error: "Wrong username or password."})
		return
	}

	token, _ := ad.sessions.create()
	setSessionCookie(w, token, ad.secure)
	ad.log.Info("admin signed in", "agent", r.UserAgent())
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (ad *Admin) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		ad.sessions.destroy(cookie.Value)
	}
	clearSessionCookie(w, ad.secure)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// userView is one account as the page shows it, including a live look at
// whether its directory is actually usable.
type userView struct {
	store.User
	Probe fsx.Probe
}

type pageData struct {
	Title    string
	Error    string
	Notice   string
	CSRF     string
	ReadOnly bool
	Users    []userView
	User     *store.User
	Probe    *fsx.Probe
	// Form holds submitted values so a rejected form comes back filled in
	// rather than blank.
	Form userForm
	// RunningAsRoot reports whether ownership can actually be applied.
	RunningAsRoot bool
	// Mounts lists paths mounted into the container that could hold user
	// files. Nothing inside a container can discover the host path a mount
	// came from, so this is shown instead of guessing at one.
	Mounts []string
	// QR and SetupURL carry a one-time sign-in code for a new device. Held only
	// for the render that produced them; the credential cannot be shown again.
	QR       template.HTML
	SetupURL string
	// HasAvatar reports whether a picture was uploaded, which decides whether
	// the page offers to remove it. AvatarVersion busts the browser's cache of
	// the preview so a new upload is visible at once.
	HasAvatar       bool
	AvatarVersion   int64
	AvatarCanonical int
}

type userForm struct {
	Username    string
	DisplayName string
	Home        string
	UID         string
	GID         string
	QuotaGB     string
}

func (ad *Admin) listUsers(w http.ResponseWriter, r *http.Request, sess *session) {
	users, err := ad.db.ListUsers(r.Context())
	if err != nil {
		ad.internalError(w, "list accounts", err)
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, userView{User: u, Probe: fsx.ProbeHome(u.Home, u.UID, u.GID)})
	}
	ad.render(w, "users.html", http.StatusOK, pageData{
		Title: "Accounts", CSRF: sess.csrf, ReadOnly: ad.readOnly,
		Users: views, Notice: r.URL.Query().Get("notice"),
		RunningAsRoot: os.Geteuid() == 0,
	})
}

func (ad *Admin) newUserForm(w http.ResponseWriter, r *http.Request, sess *session) {
	if ad.refuseWhenReadOnly(w) {
		return
	}
	ad.render(w, "user_form.html", http.StatusOK, pageData{
		Title: "Add an account", CSRF: sess.csrf,
		Form:          userForm{UID: "1026", GID: "100"},
		RunningAsRoot: os.Geteuid() == 0,
		Mounts:        fsx.DataMounts(),
	})
}

func (ad *Admin) createUser(w http.ResponseWriter, r *http.Request, sess *session) {
	if ad.refuseWhenReadOnly(w) {
		return
	}
	form, mapping, err := parseUserForm(r)
	if err != nil {
		ad.render(w, "user_form.html", http.StatusBadRequest, pageData{
			Title: "Add an account", CSRF: sess.csrf, Error: err.Error(), Form: form,
			RunningAsRoot: os.Geteuid() == 0, Mounts: fsx.DataMounts(),
		})
		return
	}

	created, err := ad.db.CreateUser(r.Context(), mapping)
	if err != nil {
		ad.render(w, "user_form.html", http.StatusBadRequest, pageData{
			Title: "Add an account", CSRF: sess.csrf, Error: err.Error(), Form: form,
			RunningAsRoot: os.Geteuid() == 0, Mounts: fsx.DataMounts(),
		})
		return
	}

	ad.log.Info("account created from the admin page",
		"user", created.Username, "home", created.Home, "uid", created.UID, "gid", created.GID)

	// Index it now. Until an account's root is in the index every request for
	// it answers 404, so without this a new account looks broken until the next
	// periodic scan comes round. Backgrounded because an existing home may
	// already hold a great deal.
	go ad.rescan(created.ID)
	// The new account has no password yet, so the edit page is where the admin
	// naturally lands next.
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=%s", created.ID,
		"Account created. Set a password before connecting a client."), http.StatusSeeOther)
}

func (ad *Admin) editUserForm(w http.ResponseWriter, r *http.Request, sess *session) {
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}
	probe := fsx.ProbeHome(u.Home, u.UID, u.GID)
	// A failure here only costs the page the offer to remove a picture, which
	// is not worth refusing to render the account over.
	uploaded, err := ad.db.AvatarVersion(r.Context(), u.ID)
	if err != nil {
		ad.log.Warn("could not read the account picture", "user", u.Username, "error", err)
	}
	ad.render(w, "user_form.html", http.StatusOK, pageData{
		Title: u.Username, CSRF: sess.csrf, ReadOnly: ad.readOnly,
		User: &u, Probe: &probe, Notice: r.URL.Query().Get("notice"),
		Form: userForm{
			Username: u.Username, DisplayName: u.DisplayName, Home: u.Home,
			UID: strconv.Itoa(u.UID), GID: strconv.Itoa(u.GID),
			QuotaGB: quotaToGB(u.Quota),
		},
		RunningAsRoot:   os.Geteuid() == 0,
		Mounts:          fsx.DataMounts(),
		HasAvatar:       !uploaded.IsZero(),
		AvatarVersion:   uploaded.UnixNano(),
		AvatarCanonical: avatar.Canonical,
	})
}

func (ad *Admin) updateUser(w http.ResponseWriter, r *http.Request, sess *session) {
	if ad.refuseWhenReadOnly(w) {
		return
	}
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}
	form, mapping, err := parseUserForm(r)
	if err == nil {
		err = ad.db.UpdateUser(r.Context(), u.ID, mapping)
	}
	if err != nil {
		probe := fsx.ProbeHome(u.Home, u.UID, u.GID)
		ad.render(w, "user_form.html", http.StatusBadRequest, pageData{
			Title: u.Username, CSRF: sess.csrf, Error: err.Error(),
			User: &u, Probe: &probe, Form: form, RunningAsRoot: os.Geteuid() == 0,
			Mounts: fsx.DataMounts(),
		})
		return
	}

	// Credentials cached against the old name would otherwise keep working.
	ad.auth.Forget(u.Username)
	ad.auth.Forget(mapping.Username)
	if u.Home != mapping.Home {
		// The open directory handle points at the old location.
		ad.storage.Forget(u.ID)
		ad.log.Info("account home changed; index dropped and will be rebuilt",
			"user", mapping.Username, "from", u.Home, "to", mapping.Home)
		go ad.rescan(u.ID)
	}
	ad.log.Info("account updated from the admin page", "user", mapping.Username)
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=Saved.", u.ID), http.StatusSeeOther)
}

func (ad *Admin) setPassword(w http.ResponseWriter, r *http.Request, sess *session) {
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}
	password := r.PostFormValue("password")
	if len(password) < 8 {
		ad.redirectWithNotice(w, r, u.ID, "Password must be at least 8 characters.")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		ad.internalError(w, "hash password", err)
		return
	}
	if err := ad.db.SetPasswordHash(r.Context(), u.ID, hash); err != nil {
		ad.internalError(w, "store password", err)
		return
	}
	ad.auth.Forget(u.Username)
	ad.credentialsChangedFor(u.ID)
	ad.log.Info("account password changed from the admin page", "user", u.Username)
	ad.redirectWithNotice(w, r, u.ID, "Password updated.")
}

func (ad *Admin) setState(w http.ResponseWriter, r *http.Request, sess *session) {
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}
	disable := r.PostFormValue("action") == "disable"
	if err := ad.db.SetDisabled(r.Context(), u.ID, disable); err != nil {
		ad.internalError(w, "change account state", err)
		return
	}
	if disable {
		ad.credentialsChangedFor(u.ID)
	}
	// Existing sessions and cached credentials must stop working at once, or
	// disabling a compromised account would not take effect until they expire.
	ad.auth.Forget(u.Username)
	ad.log.Info("account state changed from the admin page", "user", u.Username, "disabled", disable)

	notice := "Account enabled."
	if disable {
		notice = "Account disabled. Its clients can no longer connect."
	}
	http.Redirect(w, r, "/admin/users?notice="+notice, http.StatusSeeOther)
}

func (ad *Admin) deleteUser(w http.ResponseWriter, r *http.Request, sess *session) {
	if ad.refuseWhenReadOnly(w) {
		return
	}
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}
	// Typing the username is the confirmation. A delete removes credentials and
	// the whole index, and the button sits next to ordinary ones.
	if r.PostFormValue("confirm") != u.Username {
		ad.redirectWithNotice(w, r, u.ID, "Type the username exactly to confirm deletion.")
		return
	}
	if err := ad.db.DeleteUser(r.Context(), u.ID); err != nil {
		ad.internalError(w, "delete account", err)
		return
	}
	ad.auth.Forget(u.Username)
	ad.storage.Forget(u.ID)
	ad.log.Warn("account deleted from the admin page", "user", u.Username, "home", u.Home)
	http.Redirect(w, r,
		"/admin/users?notice=Account deleted. Its files were left untouched on disk.",
		http.StatusSeeOther)
}

// setUpDevice mints a credential for one device and shows it as a code to scan.
//
// Typing a server address, an account name and a long password into a phone is
// the step where setting somebody up actually fails. The clients read exactly
// the URL the pairing flow redirects to, so the same thing rendered as a code
// signs a device in with one scan.
//
// The credential is its own app password rather than the account's, so it can
// be revoked alone, and it is shown once: only its hash is kept.
func (ad *Admin) setUpDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}

	appPassword, err := auth.GenerateAppPassword()
	if err != nil {
		ad.internalError(w, "generate app password", err)
		return
	}
	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" {
		label = "Set up from the admin page"
	}
	if _, err := ad.db.CreateAppPassword(r.Context(), u.ID, label, auth.HashToken(appPassword)); err != nil {
		ad.internalError(w, "store app password", err)
		return
	}

	setupURL := auth.HandoffURL(ad.externalURL, u.Username, appPassword)
	code, err := qrcode.SVG(setupURL)
	if err != nil {
		ad.internalError(w, "render sign-in code", err)
		return
	}

	ad.log.Info("issued a device sign-in code", "user", u.Username, "label", label)
	probe := fsx.ProbeHome(u.Home, u.UID, u.GID)
	ad.render(w, "device.html", http.StatusOK, pageData{
		Title: "Set up a device", CSRF: sess.csrf, User: &u, Probe: &probe,
		QR: code, SetupURL: setupURL, RunningAsRoot: os.Geteuid() == 0,
	})
}

func (ad *Admin) scanUser(w http.ResponseWriter, r *http.Request, sess *session) {
	u, ok := ad.lookupUser(w, r)
	if !ok {
		return
	}
	go ad.rescan(u.ID)
	http.Redirect(w, r, "/admin/users?notice=Scan started.", http.StatusSeeOther)
}

// rescan reindexes one account in the background.
func (ad *Admin) rescan(userID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	u, err := ad.db.UserByID(ctx, userID)
	if err != nil {
		ad.log.Error("could not load account for scan", "error", err)
		return
	}
	if _, err := ad.scanner.ScanUser(ctx, u); err != nil {
		ad.log.Error("scan failed", "user", u.Username, "error", err)
	}
}

func (ad *Admin) lookupUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.User{}, false
	}
	u, err := ad.db.UserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return store.User{}, false
		}
		ad.internalError(w, "look up account", err)
		return store.User{}, false
	}
	return u, true
}

// refuseWhenReadOnly blocks changes while the config file owns the account list.
func (ad *Admin) refuseWhenReadOnly(w http.ResponseWriter) bool {
	if !ad.readOnly {
		return false
	}
	http.Error(w,
		"Accounts are declared in the config file, which is authoritative. "+
			"Remove the users: section to manage them here instead.",
		http.StatusConflict)
	return true
}

func (ad *Admin) redirectWithNotice(w http.ResponseWriter, r *http.Request, id int64, notice string) {
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=%s", id, notice), http.StatusSeeOther)
}

func (ad *Admin) render(w http.ResponseWriter, name string, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; "+
			"form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := ad.tmpl.ExecuteTemplate(w, name, data); err != nil {
		ad.log.Error("could not render the admin page", "template", name, "error", err)
	}
}

func (ad *Admin) internalError(w http.ResponseWriter, what string, err error) {
	ad.log.Error("admin request failed", "operation", what, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
