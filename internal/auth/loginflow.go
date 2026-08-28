package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// failedLoginDelay slows credential guessing against the pairing page. It is a
// floor, not a real rate limiter; proper per-IP limiting arrives with the rest
// of the hardening work.
const failedLoginDelay = 750 * time.Millisecond

// LoginFlow implements Nextcloud's Login Flow v2, the browser-based pairing
// handshake modern clients use by default.
//
// The client POSTs to Start, opens the returned login URL in a browser, and
// polls until the user signs in. Approval mints an app password, which the
// client collects from its next poll and then uses for every request.
type LoginFlow struct {
	db          *store.DB
	auth        *Authenticator
	externalURL string
	log         *slog.Logger
	tmpl        *template.Template
}

// NewLoginFlow builds the pairing handler.
func NewLoginFlow(db *store.DB, a *Authenticator, externalURL string, log *slog.Logger) (*LoginFlow, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &LoginFlow{db: db, auth: a, externalURL: strings.TrimRight(externalURL, "/"), log: log, tmpl: tmpl}, nil
}

type startResponse struct {
	Poll  pollInfo `json:"poll"`
	Login string   `json:"login"`
}

type pollInfo struct {
	Token    string `json:"token"`
	Endpoint string `json:"endpoint"`
}

// Start handles POST /index.php/login/v2 and opens a pairing session.
func (lf *LoginFlow) Start(w http.ResponseWriter, r *http.Request) {
	pollToken, err := randomToken()
	if err != nil {
		lf.log.Error("could not generate poll token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	loginToken, err := randomToken()
	if err != nil {
		lf.log.Error("could not generate login token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	device := describeClient(r.UserAgent())
	if err := lf.db.CreateLoginFlow(r.Context(), hashToken(pollToken), loginToken, device); err != nil {
		lf.log.Error("could not create login flow", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	lf.log.Info("pairing started", "device", device)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	//nolint:errcheck // connection gone
	json.NewEncoder(w).Encode(startResponse{
		Poll: pollInfo{
			Token:    pollToken,
			Endpoint: lf.externalURL + "/login/v2/poll",
		},
		Login: lf.externalURL + "/index.php/login/v2/flow/" + loginToken,
	})
}

type pollResponse struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

// Poll handles POST /login/v2/poll.
//
// It answers 404 while the user has not yet approved, which is the signal
// clients wait on. The same 404 covers an unknown, expired, or already-claimed
// token, so polling reveals nothing about which of those it was.
func (lf *LoginFlow) Poll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	token := r.PostFormValue("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	granted, err := lf.db.ClaimLoginFlow(r.Context(), hashToken(token))
	if err != nil {
		if !errors.Is(err, store.ErrLoginFlowPending) {
			lf.log.Error("could not claim login flow", "error", err)
		}
		http.NotFound(w, r)
		return
	}

	lf.log.Info("pairing completed", "user", granted.Username)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	//nolint:errcheck // connection gone
	json.NewEncoder(w).Encode(pollResponse{
		Server:      lf.externalURL,
		LoginName:   granted.Username,
		AppPassword: granted.AppPassword,
	})
}

type pageData struct {
	Device   string
	Action   string
	Username string
	Error    string
	// Handoff is a custom-scheme URL the browser is sent to once access is
	// granted, which is how a mobile app is handed its credentials. Empty for
	// the polling flow, where the app collects them itself.
	//
	// Typed as template.URL because html/template permits only http, https and
	// mailto in a URL attribute and rewrites anything else to a placeholder -
	// which silently turns the handover into a dead link. Declaring it safe is
	// justified by handoffURL building the whole thing: the account name is
	// already restricted to a known character set, the credential is generated
	// alphanumeric, and both are escaped on the way in.
	Handoff template.URL
}

// Page serves the browser side of pairing at
// /index.php/login/v2/flow/{token}: the sign-in form on GET, and the approval
// on POST.
func (lf *LoginFlow) Page(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	device, err := lf.db.LoginFlowUserAgent(r.Context(), token)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			lf.log.Error("could not load login flow", "error", err)
		}
		lf.renderExpired(w)
		return
	}

	data := pageData{Device: device, Action: r.URL.Path}
	if r.Method == http.MethodGet {
		lf.render(w, "login.html", http.StatusOK, data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "That request could not be read. Please try again."
		lf.render(w, "login.html", http.StatusBadRequest, data)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	data.Username = username

	user, err := lf.auth.Verify(r.Context(), username, password)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			lf.log.Error("pairing sign-in failed", "error", err)
			data.Error = "Something went wrong. Please try again."
			lf.render(w, "login.html", http.StatusInternalServerError, data)
			return
		}
		lf.log.Info("rejected pairing sign-in", "user", username, "device", device)
		time.Sleep(failedLoginDelay)
		data.Error = "Wrong username or password."
		lf.render(w, "login.html", http.StatusUnauthorized, data)
		return
	}

	appPassword, err := GenerateAppPassword()
	if err != nil {
		lf.log.Error("could not generate app password", "error", err)
		data.Error = "Something went wrong. Please try again."
		lf.render(w, "login.html", http.StatusInternalServerError, data)
		return
	}

	// Grant first. It is conditional on the session still being pending, so if
	// two approvals race, only one proceeds to mint a usable token; creating the
	// app password first would leave the loser's token behind with no owner.
	if err := lf.db.GrantLoginFlow(r.Context(), token, user.ID, appPassword); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			lf.renderExpired(w)
			return
		}
		lf.log.Error("could not grant login flow", "error", err)
		data.Error = "Something went wrong. Please try again."
		lf.render(w, "login.html", http.StatusInternalServerError, data)
		return
	}
	if _, err := lf.db.CreateAppPassword(r.Context(), user.ID, device, HashToken(appPassword)); err != nil {
		lf.log.Error("could not store app password", "user", user.Username, "error", err)
		data.Error = "Something went wrong. Please try again."
		lf.render(w, "login.html", http.StatusInternalServerError, data)
		return
	}

	lf.log.Info("device authorised", "user", user.Username, "device", device, "flow", "v2")

	// The credential is handed over here as well as through polling.
	//
	// Polling is how the desktop client collects it, and for that this page
	// only has to say the sign-in worked. A mobile app opens this page inside
	// itself, and what dismisses that browser is the navigation to the scheme
	// the app registered - so without it the app sits on a page reading
	// "Device connected" having received nothing, which is indistinguishable
	// from a hang. The same URL serves both: an app that is polling has its
	// credential already and simply comes to the foreground.
	lf.render(w, "granted.html", http.StatusOK, pageData{
		Device:  device,
		Handoff: template.URL(HandoffURL(lf.externalURL, user.Username, appPassword)),
	})
}

func (lf *LoginFlow) renderExpired(w http.ResponseWriter) {
	lf.render(w, "login.html", http.StatusNotFound, pageData{
		Device: "Unknown device",
		Error:  "This pairing link has expired or already been used. Start again from your device.",
	})
}

func (lf *LoginFlow) render(w http.ResponseWriter, name string, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// These pages accept credentials, so they must never be framed, sniffed, or
	// cached, and they need no third-party resources at all.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	// The handover navigates to a custom scheme, so the policy has to allow it
	// as a navigation target; everything else stays shut.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; "+
			"frame-ancestors 'none'; navigate-to 'self' "+handoffScheme+":")
	w.WriteHeader(status)
	if err := lf.tmpl.ExecuteTemplate(w, name, data); err != nil {
		lf.log.Error("could not render page", "template", name, "error", err)
	}
}

// PrunePairingSessions deletes expired pairing sessions. Callers run it
// periodically; the table is otherwise unbounded in the presence of clients
// that start a flow and never finish it.
func (lf *LoginFlow) PrunePairingSessions(ctx context.Context) {
	n, err := lf.db.PruneLoginFlows(ctx)
	if err != nil {
		lf.log.Error("could not prune login flows", "error", err)
		return
	}
	if n > 0 {
		lf.log.Debug("pruned expired pairing sessions", "count", n)
	}
}

// LegacyFlowPath is the pre-v2 pairing endpoint.
//
// It still matters because it is what the mobile apps use. Where the desktop
// client polls for its credentials, this flow hands them over by redirecting
// the browser to a custom scheme the app has registered - which is also what
// dismisses the in-app browser. Without it the page simply sits there after a
// successful sign-in, which is exactly what it looks like when it is missing.
const LegacyFlowPath = "/index.php/login/flow"

// handoffScheme is the URL scheme Nextcloud clients register.
const handoffScheme = "nc"

// LegacyPage serves the browser side of the pre-v2 pairing flow.
//
// There is no poll token here: the app learns nothing until the redirect at the
// end, so the credential is minted and handed over in one step.
func (lf *LoginFlow) LegacyPage(w http.ResponseWriter, r *http.Request) {
	device := describeClient(r.UserAgent())
	data := pageData{Device: device, Action: LegacyFlowPath}

	if r.Method == http.MethodGet {
		lf.render(w, "login.html", http.StatusOK, data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "That request could not be read. Please try again."
		lf.render(w, "login.html", http.StatusBadRequest, data)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	data.Username = username

	user, err := lf.auth.Verify(r.Context(), username, r.PostFormValue("password"))
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			lf.log.Error("pairing sign-in failed", "error", err)
			data.Error = "Something went wrong. Please try again."
			lf.render(w, "login.html", http.StatusInternalServerError, data)
			return
		}
		lf.log.Info("rejected pairing sign-in", "user", username, "device", device, "flow", "legacy")
		time.Sleep(failedLoginDelay)
		data.Error = "Wrong username or password."
		lf.render(w, "login.html", http.StatusUnauthorized, data)
		return
	}

	appPassword, err := GenerateAppPassword()
	if err != nil {
		lf.log.Error("could not generate app password", "error", err)
		data.Error = "Something went wrong. Please try again."
		lf.render(w, "login.html", http.StatusInternalServerError, data)
		return
	}
	if _, err := lf.db.CreateAppPassword(r.Context(), user.ID, device, HashToken(appPassword)); err != nil {
		lf.log.Error("could not store app password", "user", user.Username, "error", err)
		data.Error = "Something went wrong. Please try again."
		lf.render(w, "login.html", http.StatusInternalServerError, data)
		return
	}

	lf.log.Info("device authorised", "user", user.Username, "device", device, "flow", "legacy")

	// The redirect is the handover. It is rendered as a page rather than sent
	// as a 302 so that a browser without the scheme registered shows something
	// explaining itself instead of a bare "cannot open" error - the app is
	// dismissed by the scheme either way.
	lf.render(w, "granted.html", http.StatusOK, pageData{
		Device:  device,
		Handoff: template.URL(HandoffURL(lf.externalURL, user.Username, appPassword)),
	})
}

// HandoffURL builds the custom-scheme URL that carries credentials to a client.
//
// It is what the pairing flow redirects to, and what a sign-in code contains -
// the same URL either way, so a scan and a redirect deliver the same thing.
//
// The values are escaped the way PHP's urlencode does, which is what clients
// parse: that differs from RFC 3986 in encoding a space as "+" rather than
// "%20".
func HandoffURL(server, username, password string) string {
	return fmt.Sprintf("%s://login/server:%s&user:%s&password:%s",
		handoffScheme, server, url.QueryEscape(username), url.QueryEscape(password))
}

// randomToken returns a URL-safe secret with 256 bits of entropy.
func randomToken() (string, error) {
	return rand.Text() + rand.Text(), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// clientProduct pulls the recognisable product token out of a User-Agent.
// Nextcloud clients identify themselves as "mirall/<version>"; browsers and
// everything else fall back to a generic label.
var clientProduct = regexp.MustCompile(`(?i)\b(mirall|nextcloud[-a-z]*|owncloud[-a-z]*)/([0-9][0-9a-z.\-]*)`)

var platformHints = []struct {
	needle, name string
}{
	{"macintosh", "macOS"}, {"mac os x", "macOS"}, {"darwin", "macOS"},
	{"windows", "Windows"}, {"android", "Android"},
	{"iphone", "iOS"}, {"ipad", "iPadOS"}, {"linux", "Linux"},
}

// describeClient turns a User-Agent into something a person can recognise on
// the approval screen, since that label is the only thing standing between the
// user and approving a device they did not ask for.
func describeClient(userAgent string) string {
	if userAgent == "" {
		return "Unknown device"
	}
	lower := strings.ToLower(userAgent)

	var platform string
	for _, h := range platformHints {
		if strings.Contains(lower, h.needle) {
			platform = h.name
			break
		}
	}

	if m := clientProduct.FindStringSubmatch(userAgent); m != nil {
		name := m[1]
		if strings.EqualFold(name, "mirall") {
			name = "Nextcloud desktop"
		}
		label := name + " " + m[2]
		if platform != "" {
			label += " on " + platform
		}
		return label
	}

	if platform != "" {
		return "Unrecognised client on " + platform
	}
	return truncate(userAgent, 80)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
