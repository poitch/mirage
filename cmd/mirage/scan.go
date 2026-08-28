package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/store"
)

// cmdScan rebuilds the index from disk.
//
// The server scans on its own schedule, so this exists for the times that is
// not enough: after a bulk copy onto the NAS, or to confirm what Mirage
// currently believes is there.
func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	configPath, verbose := configFlags(fs)
	username := fs.String("user", "", "scan only this user (default: all)")
	quick := fs.Bool("quick", false,
		"use directory timestamps to find added, removed and renamed files "+
			"without stat'ing every file; misses edits made in place")
	workers := fs.Int("workers", 0,
		"directory timestamps to read at once during a quick pass "+
			"(0 uses the configured value); the useful setting depends on the "+
			"storage, so time a pass at a few values to find it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*verbose)
	cfg, db, err := setup(*configPath, log)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	// Already validated when the config loaded, so this cannot fail here.
	excluder, _ := fsx.NewExcluder(cfg.Storage.Exclude)
	storage := fsx.NewManager(cfg.Storage.FileMode.Perm(), cfg.Storage.DirMode.Perm(), excluder)
	defer storage.Close()
	scanner := index.NewScanner(db, storage, log)
	scanner.SetWorkers(cfg.Storage.ScanWorkers)
	if *workers > 0 {
		scanner.SetWorkers(*workers)
	}

	var users []store.User
	if *username != "" {
		u, err := db.UserByName(ctx, *username)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no user %q", *username)
		} else if err != nil {
			return err
		}
		users = []store.User{u}
	} else {
		if users, err = db.ListUsers(ctx); err != nil {
			return err
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tFILES\tDIRS\tSIZE\tREUSED\tSKIPPED\tTOOK")

	var failed int
	for _, u := range users {
		if u.Disabled {
			fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\tskipped (disabled)\n", u.Username)
			continue
		}
		scan := scanner.ScanUser
		if *quick {
			scan = scanner.QuickScanUser
		}
		stats, err := scan(ctx, u)
		if err != nil {
			fmt.Fprintf(w, "%s\tFAILED: %v\n", u.Username, err)
			failed++
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%d\t%d\t%s\n",
			u.Username, stats.Files, stats.Dirs, formatBytes(stats.Bytes),
			stats.Unchanged, stats.Skipped, stats.Duration.Round(1e6))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if failed > 0 {
		return fmt.Errorf("%d user(s) failed to scan", failed)
	}
	return nil
}
