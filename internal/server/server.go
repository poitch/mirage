// Package server wires the HTTP surface together and manages its lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/poitch/mirage/internal/admin"
	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/config"
	"github.com/poitch/mirage/internal/dav"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/ocs"
	"github.com/poitch/mirage/internal/push"
	"github.com/poitch/mirage/internal/store"
)

// pairingPruneInterval is how often expired pairing sessions are swept.
const pairingPruneInterval = 5 * time.Minute

// Server is the Mirage HTTP server.
type Server struct {
	cfg       *config.Config
	db        *store.DB
	log       *slog.Logger
	auth      *auth.Authenticator
	loginFlow *auth.LoginFlow
	ocs       *ocs.Service
	storage   *fsx.Manager
	scanner   *index.Scanner
	watcher   *index.Watcher
	dav       *dav.Handler
	uploads   *dav.UploadHandler
	push      *push.Hub
	admin     *admin.Admin
	http      *http.Server
}

// New builds a Server. It does not begin listening; call Run for that.
func New(ctx context.Context, cfg *config.Config, db *store.DB, log *slog.Logger) (*Server, error) {
	authenticator := auth.NewAuthenticator(db, log)

	instanceID, err := db.InstanceID(ctx)
	if err != nil {
		return nil, fmt.Errorf("read instance id: %w", err)
	}

	// Already validated when the config loaded, so this cannot fail here.
	excluder, _ := fsx.NewExcluder(cfg.Storage.Exclude)
	storage := fsx.NewManager(cfg.Storage.FileMode.Perm(), cfg.Storage.DirMode.Perm(), excluder)
	scanner := index.NewScanner(db, storage, log)
	updater := index.NewUpdater(db)
	updater.SetStorage(storage)
	watcher := index.NewWatcher(db, storage, scanner, updater, log, cfg.Storage.MaxWatches)

	// Changes reach connected clients in about a second instead of waiting for
	// the poll interval. Both the write path and the scanner report through it,
	// so a change made by a client and one made over SMB look the same.
	pushHub := push.NewHub(authenticator, db, log)
	updater.SetNotifier(pushHub)
	scanner.SetNotifier(pushHub)

	const readOnly = false
	davHandler := dav.NewHandler(db, storage, updater, scanner, log, instanceID, readOnly)
	uploadHandler := dav.NewUploadHandler(db, storage, updater, log, instanceID)

	loginFlow, err := auth.NewLoginFlow(db, authenticator, cfg.Server.ExternalURL, log)
	if err != nil {
		return nil, fmt.Errorf("build login flow: %w", err)
	}

	// Each capability stays unadvertised until it is implemented: announcing
	// one early puts a control in the client that then fails.
	features := ocs.Features{Trashbin: false, Versioning: false, Chunking: true, Push: true}

	usage := func(ctx context.Context, u store.User) (int64, error) {
		return store.UserUsage(ctx, db, u.ID)
	}
	ocsService, err := ocs.NewService(cfg.Server.AdvertisedVersion, cfg.Server.ExternalURL,
		features, authenticator, db, log, usage)
	if err != nil {
		return nil, fmt.Errorf("build OCS service: %w", err)
	}

	// Absent admin credentials leave the page unrouted entirely. An admin page
	// that appears with a blank password would be worse than none, and this
	// one can repoint any account at any directory.
	adminPage, err := admin.New(db, storage, scanner, authenticator, log,
		cfg.Server.ExternalURL, cfg.ManagesUsers())
	switch {
	case errors.Is(err, admin.ErrDisabled):
		adminPage = nil
		log.Info("admin page disabled; set " + admin.EnvPassword + " to enable it")
	case err != nil:
		return nil, fmt.Errorf("build admin page: %w", err)
	default:
		if cfg.ManagesUsers() {
			log.Warn("admin page is read-only: the config file declares accounts, " +
				"so it is authoritative; remove the users: section to manage them from the page")
		}
	}

	s := &Server{
		cfg: cfg, db: db, log: log,
		auth: authenticator, loginFlow: loginFlow, ocs: ocsService,
		storage: storage, scanner: scanner, watcher: watcher,
		dav: davHandler, uploads: uploadHandler, push: pushHub, admin: adminPage,
	}
	s.http = &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: s.routes(),
		// Sync clients hold long uploads and downloads open, so there is
		// deliberately no global write timeout; per-handler deadlines guard the
		// endpoints where one is meaningful.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	protected := s.auth.Require

	mux.HandleFunc("GET /healthz", s.health)

	// Server identification. A client that cannot read status.php never gets
	// as far as offering to log in.
	mux.HandleFunc("GET /status.php", s.ocs.Status)
	mux.HandleFunc("GET /index.php/204", s.ocs.NoContent)

	// Capabilities are static and reveal nothing about any account, so they are
	// served unauthenticated. Clients ask both before and after pairing.
	mux.HandleFunc("GET /ocs/v1.php/cloud/capabilities", s.ocs.Capabilities(ocs.V1))
	mux.HandleFunc("GET /ocs/v2.php/cloud/capabilities", s.ocs.Capabilities(ocs.V2))

	mux.Handle("GET /ocs/v1.php/cloud/user", protected(s.ocs.User(ocs.V1)))
	mux.Handle("GET /ocs/v2.php/cloud/user", protected(s.ocs.User(ocs.V2)))

	// Clients call this when an account is removed, to revoke their own token.
	mux.Handle("DELETE /ocs/v2.php/core/apppassword", protected(s.ocs.DeleteAppPassword(ocs.V2)))

	// The provisioning API. The mobile apps poll this for the account screen.
	mux.Handle("GET /ocs/v1.php/cloud/users/{userid}", protected(s.ocs.UserDetails(ocs.V1)))
	mux.Handle("GET /ocs/v2.php/cloud/users/{userid}", protected(s.ocs.UserDetails(ocs.V2)))

	// Endpoints the apps ask about on every connection. Each has an honest
	// answer, and answering is what stops the app retrying.
	mux.Handle("GET /ocs/v1.php/core/navigation/apps", protected(s.ocs.NavigationApps(ocs.V1)))
	mux.Handle("GET /ocs/v2.php/core/navigation/apps", protected(s.ocs.NavigationApps(ocs.V2)))
	mux.Handle("GET /ocs/v1.php/apps/terms_of_service/terms", protected(s.ocs.Terms(ocs.V1)))
	mux.Handle("GET /ocs/v2.php/apps/terms_of_service/terms", protected(s.ocs.Terms(ocs.V2)))
	mux.Handle("POST /ocs/v2.php/apps/notifications/api/v2/push", protected(s.ocs.PushRegistration(ocs.V2)))
	mux.Handle("DELETE /ocs/v2.php/apps/notifications/api/v2/push", protected(s.ocs.PushRegistration(ocs.V2)))

	// Avatars. Clients ask for these on several paths; all of them draw the
	// same generated image.
	for _, pattern := range []string{
		"/remote.php/dav/avatars/{user}/{size}",
		"/index.php/avatar/{user}/{size}",
		"/avatar/{user}/{size}",
	} {
		mux.Handle("GET "+pattern, protected(http.HandlerFunc(s.ocs.Avatar)))
		mux.Handle("HEAD "+pattern, protected(http.HandlerFunc(s.ocs.Avatar)))
		// The apps append /dark when the device is in dark mode. The generated
		// mark reads the same either way, so both return it.
		mux.Handle("GET "+pattern+"/dark", protected(http.HandlerFunc(s.ocs.Avatar)))
	}

	// WebDAV. Methods are dispatched inside the handler rather than by the mux
	// so that PROPFIND and the other extension verbs share one route.
	mux.Handle("/remote.php/dav/files/{user}/{path...}", protected(s.dav))
	mux.Handle("/remote.php/dav/files/{user}", protected(s.dav))
	// The pre-DAV root, still probed by desktop clients during setup.
	mux.Handle("/remote.php/webdav/{path...}", protected(http.HandlerFunc(s.dav.ServeLegacy)))
	mux.Handle("/remote.php/webdav", protected(http.HandlerFunc(s.dav.ServeLegacy)))

	// Chunked upload, which clients use for anything large.
	mux.Handle("/remote.php/dav/uploads/{user}/{path...}", protected(s.uploads))

	// Search. Clients send this to the DAV root rather than to a collection.
	mux.Handle(dav.SearchPath, protected(http.HandlerFunc(s.dav.ServeSearch)))

	// notify_push. The websocket is not behind the Basic auth middleware: the
	// protocol authenticates itself after the handshake, and a browser cannot
	// attach an Authorization header to a websocket upgrade anyway.
	mux.HandleFunc("GET /push/ws", s.push.ServeWS)
	mux.Handle("POST /index.php/apps/notify_push/pre_auth", protected(http.HandlerFunc(s.push.PreAuth)))
	mux.Handle("GET /index.php/apps/notify_push/pre_auth", protected(http.HandlerFunc(s.push.PreAuth)))
	mux.Handle("POST /apps/notify_push/pre_auth", protected(http.HandlerFunc(s.push.PreAuth)))
	mux.Handle("GET /apps/notify_push/pre_auth", protected(http.HandlerFunc(s.push.PreAuth)))

	// Login Flow v2. The poll endpoint is advertised without the index.php
	// prefix but clients have historically used both, so both are routed.
	mux.HandleFunc("POST /index.php/login/v2", s.loginFlow.Start)
	mux.HandleFunc("POST /login/v2/poll", s.loginFlow.Poll)
	mux.HandleFunc("POST /index.php/login/v2/poll", s.loginFlow.Poll)
	mux.HandleFunc("GET /index.php/login/v2/flow/{token}", s.loginFlow.Page)
	mux.HandleFunc("POST /index.php/login/v2/flow/{token}", s.loginFlow.Page)

	// The pre-v2 pairing flow, which the mobile apps use. It hands credentials
	// over by redirecting to a scheme the app registered, rather than by having
	// the app poll for them.
	mux.HandleFunc("GET "+auth.LegacyFlowPath, s.loginFlow.LegacyPage)
	mux.HandleFunc("POST "+auth.LegacyFlowPath, s.loginFlow.LegacyPage)

	if s.admin != nil {
		s.admin.Routes(mux)
	}

	// Nextcloud clients probe undocumented paths and shift between releases.
	// Logging every unrouted request turns "the client mysteriously fails to
	// connect" into a single line naming the endpoint we are missing.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("unhandled request",
			"method", r.Method, "path", r.URL.Path, "agent", r.UserAgent())
		http.NotFound(w, r)
	})

	return s.withRequestLogging(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	defer s.storage.Close() //nolint:errcheck // shutting down regardless

	go s.prunePairingSessions(ctx)
	go s.pruneUploads(ctx)
	go s.rescan(ctx)
	go s.watch(ctx)

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.cfg.Server.Listen, "external_url", s.cfg.Server.ExternalURL)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// rescan keeps the index in step with the filesystem.
//
// The first pass runs at startup so a server that was down while files changed
// catches up before clients connect. It runs in the background: a large home
// directory can take a while, and refusing connections until it finishes would
// be worse than serving a briefly stale index.
//
// Until the filesystem watcher lands, this interval is the only thing that
// surfaces a file dropped in over SMB, so it is also the sync latency.
func (s *Server) rescan(ctx context.Context) {
	// Before anything can be scanning, settle what happened to the last one. A
	// record still saying "running" was left by a process that stopped without
	// being able to say so - which is exactly the case a timeout heuristic
	// cannot distinguish from a scan that is simply busy.
	if p, was, err := index.ReconcileInterrupted(ctx, s.db); err != nil {
		s.log.Warn("could not reconcile the previous scan", "error", err)
	} else if was {
		s.log.Warn("the previous scan did not finish; starting again",
			"user", p.User, "reached_files", p.Files, "reached_dirs", p.Dirs,
			"last_at", p.Current)
	}

	// A quick pass for accounts that already have an index, a full one for
	// those that do not. A restart should not cost a walk of the whole share.
	if err := s.scanner.StartupScan(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Error("startup scan failed", "error", err)
	}

	if s.cfg.Storage.RescanInterval.Duration() <= 0 {
		s.log.Warn("periodic rescan is disabled; changes made outside Mirage will not be seen")
		return
	}
	// A timer reset after each scan, deliberately not a ticker. A ticker fires
	// on a fixed schedule, so a scan that takes longer than the interval leaves
	// a tick already waiting when it finishes and the next scan begins at once.
	// On a large share, where a pass takes far longer than any sensible
	// interval, that means scanning without pause forever - the disks never go
	// quiet and nothing ever says why. Resetting after the scan makes the
	// interval mean what it reads like: the gap between scans.
	go s.quickRescan(ctx)

	interval := s.cfg.Storage.RescanInterval.Duration()
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			started := time.Now()
			if err := s.scanner.ScanAll(ctx, "periodic rescan"); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Error("rescan failed", "error", err)
			}
			timer.Reset(s.nextGap("full rescan", interval, time.Since(started)))
		}
	}
}

// maxDutyCycle bounds the share of time that may be spent scanning.
//
// A pass that takes longer than the gap configured after it would otherwise
// start the next one the moment it finished, and on a large share that is
// scanning without pause - disks that never spin down, for a share whose
// contents are mostly archives that have not changed in years. Rather than
// leave that to be noticed in a log, the gap grows to keep the duty cycle
// under this.
const maxDutyCycle = 0.25

// nextGap returns how long to wait before the next pass of this kind.
//
// Normally the configured interval. When a pass took long enough that
// honouring the interval would mean scanning most of the time, the gap widens
// instead, and says so - because the setting can no longer mean what it reads
// like, and only the operator can decide whether to accept the slower cadence
// or reduce the work.
func (s *Server) nextGap(kind string, interval, took time.Duration) time.Duration {
	floor := time.Duration(float64(took) * (1 - maxDutyCycle) / maxDutyCycle)
	if floor <= interval {
		return interval
	}
	s.log.Warn("a "+kind+" takes long enough that the configured interval would mean "+
		"scanning almost continuously; waiting longer instead",
		"took", took.Round(time.Second),
		"configured_interval", interval,
		"waiting", floor.Round(time.Second),
		"hint", "this is the cost of the share as it stands; lower it with "+
			"storage.exclude, or accept the slower cadence")
	return floor
}

// quickRescan runs the cheap pass that finds files added, removed or renamed.
//
// This is the one that decides how long a file dropped over SMB takes to reach
// a client on a share too large to watch every directory of. It reads directory
// timestamps rather than stat'ing every file, so it can run often enough to
// matter without keeping the disks busy.
func (s *Server) quickRescan(ctx context.Context) {
	interval := s.cfg.Storage.QuickRescanInterval.Duration()
	if interval <= 0 {
		s.log.Info("quick rescan disabled; changes made outside Mirage wait for " +
			"the filesystem watcher or the full rescan")
		return
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			started := time.Now()
			if err := s.scanner.QuickScanAll(ctx, "quick rescan"); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Error("quick rescan failed", "error", err)
			}
			timer.Reset(s.nextGap("quick rescan", interval, time.Since(started)))
		}
	}
}

// watch keeps the index in step with the filesystem as changes happen.
//
// The watcher is started after the initial scan is under way rather than
// instead of it: it reports changes from now on, and says nothing about what
// happened while the server was down.
func (s *Server) watch(ctx context.Context) {
	if !s.cfg.Storage.Watcher {
		s.log.Info("filesystem watcher disabled; changes made outside Mirage will be " +
			"picked up by the periodic rescan only")
		return
	}
	if err := s.watcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// Losing the watcher costs latency, not correctness: the rescan still
		// reconciles everything. So it is a warning, not a reason to stop.
		s.log.Warn("filesystem watcher stopped; falling back to the periodic rescan",
			"error", err, "rescan_interval", s.cfg.Storage.RescanInterval)
	}
}

// abandonedUploadTTL is how long an untouched transfer is kept. It matches the
// window Nextcloud allows, and clients do not expect to resume beyond it.
const abandonedUploadTTL = 24 * time.Hour

// uploadPruneInterval is how often abandoned transfers are swept.
const uploadPruneInterval = time.Hour

// pruneUploads discards transfers that were started and never finished.
//
// Without this they are permanent: a client that begins a large upload and
// never returns would leave its chunks occupying the user's disk forever.
func (s *Server) pruneUploads(ctx context.Context) {
	sweep := func() {
		users, err := s.db.ListUsers(ctx)
		if err != nil {
			s.log.Error("could not list users to prune uploads", "error", err)
			return
		}
		cutoff := time.Now().Add(-abandonedUploadTTL)
		for _, u := range users {
			if u.Disabled {
				continue
			}
			st, err := s.storage.For(u.ID, u.Home, u.UID, u.GID)
			if err != nil {
				continue
			}
			if n, err := st.PruneUploads(cutoff); err != nil {
				s.log.Warn("could not prune abandoned uploads", "user", u.Username, "error", err)
			} else if n > 0 {
				s.log.Info("discarded abandoned uploads", "user", u.Username, "count", n)
			}
		}
	}

	ticker := time.NewTicker(uploadPruneInterval)
	defer ticker.Stop()
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// prunePairingSessions sweeps abandoned pairing sessions until ctx is done.
func (s *Server) prunePairingSessions(ctx context.Context) {
	ticker := time.NewTicker(pairingPruneInterval)
	defer ticker.Stop()
	s.loginFlow.PrunePairingSessions(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.loginFlow.PrunePairingSessions(ctx)
		}
	}
}
