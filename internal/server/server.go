// Package server wires the HTTP surface together and manages its lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/config"
	"github.com/poitch/mirage/internal/dav"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/ocs"
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
	dav       *dav.Handler
	http      *http.Server
}

// New builds a Server. It does not begin listening; call Run for that.
func New(ctx context.Context, cfg *config.Config, db *store.DB, log *slog.Logger) (*Server, error) {
	authenticator := auth.NewAuthenticator(db, log)

	instanceID, err := db.InstanceID(ctx)
	if err != nil {
		return nil, fmt.Errorf("read instance id: %w", err)
	}

	storage := fsx.NewManager(cfg.Storage.FileMode.Perm(), cfg.Storage.DirMode.Perm())
	scanner := index.NewScanner(db, storage, log)
	updater := index.NewUpdater(db)

	const readOnly = false
	davHandler := dav.NewHandler(db, storage, updater, scanner, log, instanceID, readOnly)

	loginFlow, err := auth.NewLoginFlow(db, authenticator, cfg.Server.ExternalURL, log)
	if err != nil {
		return nil, fmt.Errorf("build login flow: %w", err)
	}

	// Each capability stays unadvertised until it is implemented: announcing
	// one early puts a control in the client that then fails.
	features := ocs.Features{Trashbin: false, Versioning: false, Chunking: false}

	usage := func(ctx context.Context, u store.User) (int64, error) {
		return store.UserUsage(ctx, db, u.ID)
	}
	ocsService, err := ocs.NewService(cfg.Server.AdvertisedVersion, features, authenticator, db, log, usage)
	if err != nil {
		return nil, fmt.Errorf("build OCS service: %w", err)
	}

	s := &Server{
		cfg: cfg, db: db, log: log,
		auth: authenticator, loginFlow: loginFlow, ocs: ocsService,
		storage: storage, scanner: scanner, dav: davHandler,
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

	// WebDAV. Methods are dispatched inside the handler rather than by the mux
	// so that PROPFIND and the other extension verbs share one route.
	mux.Handle("/remote.php/dav/files/{user}/{path...}", protected(s.dav))
	mux.Handle("/remote.php/dav/files/{user}", protected(s.dav))
	// The pre-DAV root, still probed by desktop clients during setup.
	mux.Handle("/remote.php/webdav/{path...}", protected(http.HandlerFunc(s.dav.ServeLegacy)))
	mux.Handle("/remote.php/webdav", protected(http.HandlerFunc(s.dav.ServeLegacy)))

	// Login Flow v2. The poll endpoint is advertised without the index.php
	// prefix but clients have historically used both, so both are routed.
	mux.HandleFunc("POST /index.php/login/v2", s.loginFlow.Start)
	mux.HandleFunc("POST /login/v2/poll", s.loginFlow.Poll)
	mux.HandleFunc("POST /index.php/login/v2/poll", s.loginFlow.Poll)
	mux.HandleFunc("GET /index.php/login/v2/flow/{token}", s.loginFlow.Page)
	mux.HandleFunc("POST /index.php/login/v2/flow/{token}", s.loginFlow.Page)

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
	go s.rescan(ctx)

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
	if err := s.scanner.ScanAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Error("initial scan failed", "error", err)
	}

	if s.cfg.Storage.RescanInterval.Duration() <= 0 {
		s.log.Warn("periodic rescan is disabled; changes made outside Mirage will not be seen")
		return
	}
	ticker := time.NewTicker(s.cfg.Storage.RescanInterval.Duration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.scanner.ScanAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Error("rescan failed", "error", err)
			}
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
