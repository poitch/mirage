package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/preview"
	"github.com/poitch/mirage/internal/store"
	"github.com/poitch/mirage/internal/versions"
)

//go:embed templates/*.html
var templateFS embed.FS

// Paths the browser view answers on.
const (
	// BrowsePath is where a person lands. It is also where Nextcloud clients
	// send somebody who clicks a search result, which is the reason any of this
	// exists.
	BrowsePath   = "/index.php/apps/files/"
	rootPath     = "/web/"
	loginPath    = "/web/login"
	logoutPath   = "/web/logout"
	downloadPath = "/web/download/"
)

// failedLoginDelay slows a wrong password down enough to make guessing at one
// over the network pointless.
const failedLoginDelay = 750 * time.Millisecond

// Site serves the browser view of an account's files.
type Site struct {
	db       *store.DB
	auth     *auth.Authenticator
	storage  *fsx.Manager
	log      *slog.Logger
	tmpl     *template.Template
	sessions *sessionStore
	secure   bool
	// externalURL is how a device reaches this server, which is what a sign-in
	// code has to carry.
	externalURL string
	// previews turns thumbnails on. Nil leaves listings showing icons.
	previews *preview.Handler
	// scanner and updater keep the index in step with what this page changes,
	// so a restored file is visible to clients at once rather than at the next
	// scan - which would look like the restore had failed.
	scanner *index.Scanner
	updater *index.Updater
	// keeper puts earlier copies aside, shared with the sync endpoints so that
	// restoring from here and from a phone leave the same history.
	keeper *versions.Keeper
	// trashRetention and versionsEnabled mirror what the sync endpoints allow,
	// so the page never offers a control that would fail.
	trashEnabled    bool
	versionsEnabled bool
}

// SetFeatures tells the page which of the optional features are on, so it can
// stop offering the ones that are not.
func (s *Site) SetFeatures(trash bool, keeper *versions.Keeper, previews *preview.Handler) {
	s.trashEnabled = trash
	s.keeper = keeper
	s.versionsEnabled = keeper.Enabled()
	s.previews = previews
}

// New builds the browser view. secure marks the session cookie Secure, which is
// correct behind TLS and would make signing in impossible without it.
func New(db *store.DB, a *auth.Authenticator, storage *fsx.Manager,
	scanner *index.Scanner, updater *index.Updater, externalURL string,
	log *slog.Logger) (*Site, error) {

	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Site{
		db: db, auth: a, storage: storage, log: log, tmpl: tmpl,
		scanner: scanner, updater: updater,
		sessions:    newSessionStore(),
		secure:      strings.HasPrefix(externalURL, "https://"),
		externalURL: strings.TrimRight(externalURL, "/"),
	}, nil
}

// ForgetSessions signs an account out everywhere, for when its password changes
// or it is disabled underneath it.
func (s *Site) ForgetSessions(userID int64) { s.sessions.destroyFor(userID) }

// Routes registers the browser view.
func (s *Site) Routes(mux *http.ServeMux) {
	// The address somebody is handed is the root, and until now that was a 404.
	mux.HandleFunc("GET /{$}", s.landing)
	mux.HandleFunc("GET "+loginPath, s.loginPage)
	mux.HandleFunc("POST "+loginPath, s.login)
	mux.HandleFunc("POST "+logoutPath, s.logout)
	mux.Handle("GET "+rootPath, s.guard(s.browse))
	mux.Handle("GET "+downloadPath+"{path...}", s.guard(s.download))
	mux.Handle("GET /web/search", s.guard(s.search))
	mux.Handle("GET /web/thumb/{id}", s.guard(s.thumbnail))

	mux.Handle("GET /web/trash", s.guard(s.trash))
	mux.Handle("POST /web/trash/restore", s.guard(s.restoreTrash))
	mux.Handle("POST /web/trash/delete", s.guard(s.deleteTrash))

	mux.Handle("GET /web/versions/{id}", s.guard(s.versionsPage))
	mux.Handle("GET /web/versions/{id}/{stamp}", s.guard(s.downloadVersion))
	mux.Handle("POST /web/versions/restore", s.guard(s.restoreVersion))

	mux.Handle("GET /web/profile", s.guard(s.profile))
	mux.Handle("POST /web/profile/password", s.guard(s.changePassword))
	mux.Handle("POST /web/profile/device", s.guard(s.addDevice))
	mux.Handle("POST /web/profile/device/revoke", s.guard(s.revokeDevice))

	// Where clients send a search result. Kept as its own route because it is
	// the client's address, not this application's, and it carries dir and
	// scrollto rather than a path.
	mux.Handle("GET "+BrowsePath, s.guard(s.browseFromSearch))
}

// guard requires a session, and a CSRF token on writes.
func (s *Site) guard(h func(http.ResponseWriter, *http.Request, *session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			s.redirectToLogin(w, r)
			return
		}
		sess, ok := s.sessions.lookup(cookie.Value)
		if !ok {
			clearSessionCookie(w, s.secure)
			s.redirectToLogin(w, r)
			return
		}
		// The account may have been disabled since the session started.
		user, err := s.db.UserByID(r.Context(), sess.userID)
		if err != nil || user.Disabled {
			s.sessions.destroy(cookie.Value)
			clearSessionCookie(w, s.secure)
			s.redirectToLogin(w, r)
			return
		}
		if r.Method == http.MethodPost && !sess.validCSRF(r.FormValue("csrf")) {
			http.Error(w, "invalid form token; reload the page and try again", http.StatusForbidden)
			return
		}
		h(w, r, sess)
	})
}

// pageData is what every template is given.
type pageData struct {
	// Lang is the language this page is rendered in, chosen from what the
	// browser asked for.
	Lang        lang
	Title       string
	Username    string
	DisplayName string
	CSRF        string
	Error       string
	Notice      string
	Next        string
	Crumbs      []crumb
	Entries     []entry
	Devices     []device

	// Query and Searching drive the search box and its results.
	Query     string
	Searching bool

	// QR and SetupURL carry a one-time device code. Held only for the render
	// that produced them; the credential cannot be shown again.
	QR       template.HTML
	SetupURL template.URL

	// The landing page tells somebody how to connect a device.
	PlatformName string
	ClientLabel  string
	ClientURL    string
	ServerURL    string
	OtherClients []client

	// The versions page describes one file.
	FileID          int64
	ParentURL       string
	ParentName      string
	DownloadURL     string
	CurrentSize     string
	CurrentModified string
}

type crumb struct {
	Name    string
	URL     string
	Current bool
}

type entry struct {
	Name      string
	URL       string
	IsDir     bool
	Size      string
	Modified  string
	Highlight bool
	// Where names the folder an entry came from, shown when a listing is not
	// itself a folder - search results and the trash.
	Where string
	// ThumbURL is set when a picture can be shown instead of an icon.
	ThumbURL string
	// VersionsURL is set when a file has earlier copies to look at.
	VersionsURL string
	// Token addresses the entry in a form post: a trash entry name, or a
	// version timestamp.
	Token string
}

func (s *Site) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(next), http.StatusSeeOther)
}

func (s *Site) loginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := s.sessions.lookup(cookie.Value); ok {
			http.Redirect(w, r, rootPath, http.StatusSeeOther)
			return
		}
	}
	s.render(w, r, "login.html", http.StatusOK, pageData{
		Title: s.t(r, "login.title"), Next: safeNext(r.URL.Query().Get("next")),
	})
}

func (s *Site) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, r, "login.html", http.StatusBadRequest,
			pageData{Title: s.t(r, "login.title"), Error: s.t(r, "login.unreadable")})
		return
	}
	username := r.PostFormValue("username")
	next := safeNext(r.PostFormValue("next"))

	// Verify accepts the account password and any of its app passwords, which
	// is the same credential the sync client was given.
	user, err := s.auth.Verify(r.Context(), username, r.PostFormValue("password"))
	if err != nil {
		if !errors.Is(err, auth.ErrUnauthorized) {
			s.log.Error("could not check a sign-in", "username", username, "error", err)
		} else {
			s.log.Info("rejected a web sign-in", "username", username, "agent", r.UserAgent())
		}
		time.Sleep(failedLoginDelay)
		s.render(w, r, "login.html", http.StatusUnauthorized, pageData{
			Title: s.t(r, "login.title"), Error: s.t(r, "login.wrong"), Next: next,
		})
		return
	}

	token, _ := s.sessions.create(user.ID, user.Username)
	setSessionCookie(w, token, s.secure)
	s.log.Info("signed in to the web view", "user", user.Username, "agent", r.UserAgent())
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Site) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		// The token alone is enough to end its own session, and requiring a
		// valid CSRF token here would leave somebody stuck on a stale page.
		s.sessions.destroy(cookie.Value)
	}
	clearSessionCookie(w, s.secure)
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

// safeNext keeps a redirect target local.
//
// It is taken from the query string, so without this the login page would
// forward somebody to any address an attacker put in a link.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return rootPath
	}
	return next
}

// browseFromSearch renders the folder a search result named.
//
// Clients address this with dir and scrollto rather than a path, because it is
// Nextcloud's own address for "show me this file".
func (s *Site) browseFromSearch(w http.ResponseWriter, r *http.Request, sess *session) {
	q := r.URL.Query()
	s.renderFolder(w, r, sess, q.Get("dir"), q.Get("scrollto"))
}

func (s *Site) browse(w http.ResponseWriter, r *http.Request, sess *session) {
	q := r.URL.Query()
	s.renderFolder(w, r, sess, q.Get("path"), q.Get("highlight"))
}

// renderFolder lists one directory.
func (s *Site) renderFolder(w http.ResponseWriter, r *http.Request, sess *session, dir, highlight string) {
	clean, err := fsx.CleanPath(dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	node, err := store.NodeByPath(r.Context(), s.db, sess.userID, clean)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.internalError(w, "look up folder", err)
		return
	}
	// A link to a file rather than a folder is a reasonable thing to follow;
	// send it to the download instead of refusing.
	if !node.IsDir {
		http.Redirect(w, r, downloadURL(clean), http.StatusSeeOther)
		return
	}

	children, err := store.ChildNodes(r.Context(), s.db, node.ID)
	if err != nil {
		s.internalError(w, "list folder", err)
		return
	}
	// Folders first, then by name, which is the order every file manager uses
	// and the one people expect to read.
	sort.Slice(children, func(i, j int) bool {
		if children[i].IsDir != children[j].IsDir {
			return children[i].IsDir
		}
		return strings.ToLower(children[i].Name) < strings.ToLower(children[j].Name)
	})

	entries := make([]entry, 0, len(children))
	for _, c := range children {
		e := s.entryFor(sess, languageFor(r), c)
		e.Highlight = highlight != "" && c.Name == highlight
		entries = append(entries, e)
	}

	s.render(w, r, "browse.html", http.StatusOK, pageData{
		Title:    titleFor(clean),
		Username: sess.username,
		CSRF:     sess.csrf,
		Notice:   r.URL.Query().Get("notice"),
		Crumbs:   crumbsFor(clean, s.t(r, "browse.root")),
		Entries:  entries,
	})
}

// download streams one file.
func (s *Site) download(w http.ResponseWriter, r *http.Request, sess *session) {
	clean, err := fsx.CleanPath(r.PathValue("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	user, err := s.db.UserByID(r.Context(), sess.userID)
	if err != nil {
		s.internalError(w, "look up account", err)
		return
	}
	node, err := store.NodeByPath(r.Context(), s.db, user.ID, clean)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.internalError(w, "look up file", err)
		return
	}
	if node.IsDir {
		http.Redirect(w, r, browseURL(clean, ""), http.StatusSeeOther)
		return
	}

	// Opened through the account's own confined root, so a browser session can
	// reach exactly what a sync client for this account can reach.
	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		s.internalError(w, "open storage", err)
		return
	}
	f, err := st.Open(clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.internalError(w, "stat file", err)
		return
	}

	// Always an attachment, and never sniffed: these are somebody's own files
	// and an HTML one served inline would run as a page on this origin.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", contentDisposition(node.Name))
	w.Header().Set("Cache-Control", "private, no-store")
	// ServeContent handles Range and If-Range, so a large download resumes.
	http.ServeContent(w, r, node.Name, info.ModTime(), f)
}

func (s *Site) render(w http.ResponseWriter, r *http.Request, name string, status int, data pageData) {
	// Set here rather than in each handler, because a page that forgot would
	// silently fall back to English and nobody would notice until a French
	// reader did.
	if data.Lang == "" {
		data.Lang = languageFor(r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("could not render a page", "template", name, "error", err)
	}
}

func (s *Site) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error("web request failed", "operation", what, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// browseURL addresses a folder.
func browseURL(p, highlight string) string {
	q := url.Values{}
	if p != "." && p != "" {
		q.Set("path", p)
	}
	if highlight != "" {
		q.Set("highlight", highlight)
	}
	if len(q) == 0 {
		return rootPath
	}
	return rootPath + "?" + q.Encode()
}

// downloadURL addresses a file. The path is in the URL path rather than a
// query parameter so that the browser proposes the right filename.
func downloadURL(p string) string {
	return downloadPath + (&url.URL{Path: p}).EscapedPath()
}

// contentDisposition names the download, in both the plain and the encoded
// form, so that a name with an accent in it survives every browser.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		ascii, url.PathEscape(name))
}

func titleFor(p string) string {
	if p == "." || p == "" {
		return "Files"
	}
	return path.Base(p)
}

// crumbsFor builds the trail back to the top.
func crumbsFor(p, root string) []crumb {
	out := []crumb{{Name: root, URL: rootPath}}
	if p == "." || p == "" {
		out[0].Current = true
		return out
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		out = append(out, crumb{
			Name:    part,
			URL:     browseURL(strings.Join(parts[:i+1], "/"), ""),
			Current: i == len(parts)-1,
		})
	}
	return out
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
