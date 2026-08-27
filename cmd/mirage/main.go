// Command mirage runs the Mirage file sync server and its administrative tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/poitch/mirage/internal/config"
	"github.com/poitch/mirage/internal/server"
	"github.com/poitch/mirage/internal/store"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `mirage - Nextcloud-compatible file sync server

Usage:
  mirage <command> [flags]

Commands:
  serve      Run the server
  doctor     Check the config and the health of each user's storage
  scan       Rebuild the file index from disk
  ls         Show what is indexed for an account, and what is not
  user       Inspect accounts and manage credentials
  version    Print the version

User accounts and their NAS directory mappings are defined in the config file,
which is the single source of truth for them. Use "mirage doctor" after editing
it to confirm the mapping is sound.

Run "mirage <command> -h" for command-specific flags.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mirage: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		return cmdServe(args)
	case "doctor":
		return cmdDoctor(args)
	case "scan":
		return cmdScan(args)
	case "ls":
		return cmdLs(args)
	case "user":
		return cmdUser(args)
	case "version":
		fmt.Println("mirage " + version)
		return nil
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// configFlags registers the flags every command shares.
func configFlags(fs *flag.FlagSet) (configPath *string, verbose *bool) {
	configPath = fs.String("config", envOr("MIRAGE_CONFIG", "/etc/mirage/mirage.yaml"), "path to the config file")
	verbose = fs.Bool("v", false, "enable debug logging")
	return configPath, verbose
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// setup performs the load-config, open-database, reconcile-users sequence that
// every command needs before it can do anything useful.
func setup(configPath string, log *slog.Logger) (*config.Config, *store.DB, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}

	db, err := store.Open(context.Background(), cfg.Database.Path)
	if err != nil {
		return nil, nil, err
	}

	// When the config file declares no accounts, it is not managing them and
	// the database stands alone. Reconciling against an empty list would
	// disable every account the admin page created.
	if !cfg.ManagesUsers() {
		return cfg, db, nil
	}

	mappings := make([]store.UserMapping, len(cfg.Users))
	for i, u := range cfg.Users {
		mappings[i] = store.UserMapping{
			Username:    u.Username,
			DisplayName: u.DisplayName,
			Home:        u.Home,
			UID:         u.UID,
			GID:         u.GID,
			Quota:       u.Quota,
		}
	}
	res, err := db.ReconcileUsers(context.Background(), mappings)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("reconcile users from config: %w", err)
	}
	for _, name := range res.Created {
		log.Info("user created from config", "user", name)
	}
	for _, name := range res.Updated {
		log.Info("user mapping updated from config", "user", name)
	}
	for _, name := range res.Disabled {
		log.Warn("user disabled: no longer present in config", "user", name)
	}
	for _, name := range res.Reindex {
		log.Warn("home directory changed; index dropped and must be rebuilt", "user", name)
	}
	return cfg, db, nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath, verbose := configFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*verbose)
	cfg, db, err := setup(*configPath, log)
	if err != nil {
		return err
	}
	defer db.Close()

	log.Info("mirage starting", "version", version, "config", *configPath,
		"database", db.Path(), "users", len(cfg.Users))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, db, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
