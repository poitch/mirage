// Package server wires the HTTP surface together and manages its lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/poitch/mirage/internal/config"
	"github.com/poitch/mirage/internal/store"
)

// Server is the Mirage HTTP server.
type Server struct {
	cfg  *config.Config
	db   *store.DB
	log  *slog.Logger
	http *http.Server
}

// New builds a Server. It does not begin listening; call Run for that.
func New(cfg *config.Config, db *store.DB, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, db: db, log: log}
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
	return s
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.db.PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	// Nextcloud clients probe undocumented paths and shift between releases.
	// Logging every unrouted request turns "the client mysteriously fails to
	// connect" into a single line naming the endpoint we are missing.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("unhandled request",
			"method", r.Method, "path", r.URL.Path, "agent", r.UserAgent())
		http.NotFound(w, r)
	})

	return mux
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
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
