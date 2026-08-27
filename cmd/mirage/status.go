package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/poitch/mirage/internal/index"
	"github.com/poitch/mirage/internal/store"
)

// cmdStatus reports on a scan that another process is running.
//
// A first scan of a large share takes a long time and, without this, gives no
// sign of progress beyond the disk being audibly busy. Progress is read from
// the database because the server doing the scanning is a different process
// from the one being asked.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath, verbose := configFlags(fs)
	watch := fs.Bool("watch", false, "keep printing until the scan finishes")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*verbose)
	_, db, err := setup(*configPath, log)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	for {
		done, err := printStatus(ctx, db)
		if err != nil {
			return err
		}
		if !*watch || done {
			return nil
		}
		time.Sleep(5 * time.Second)
		fmt.Println()
	}
}

// printStatus writes one snapshot, reporting whether there is nothing left to
// wait for.
func printStatus(ctx context.Context, db *store.DB) (done bool, err error) {
	p, ok, err := index.ScanProgress(ctx, db)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Println("No scan has run yet.")
		return true, nil
	}

	switch {
	case p.Done:
		fmt.Printf("Scan of %s finished.\n", p.User)
		fmt.Printf("  %d files, %d directories, %s\n", p.Files, p.Dirs, formatBytes(p.Bytes))
		fmt.Printf("  took %s\n", p.Elapsed().Round(time.Second))
		return true, nil

	case p.Stale():
		// Progress stops being written the moment the process does, so an old
		// timestamp on an unfinished scan means it was interrupted - almost
		// always by a restart, which begins the walk again.
		fmt.Printf("A scan of %s was interrupted, %s ago.\n", p.User,
			time.Since(p.UpdatedAt).Round(time.Second))
		fmt.Printf("  reached %d files, %d directories, %s\n", p.Files, p.Dirs, formatBytes(p.Bytes))
		fmt.Printf("  last at %s\n", p.Current)
		fmt.Println("  Restarting the server begins the walk again; indexed entries are kept.")
		return true, nil

	default:
		fmt.Printf("Scanning %s...\n", p.User)
		fmt.Printf("  %d files, %d directories, %s so far\n", p.Files, p.Dirs, formatBytes(p.Bytes))
		fmt.Printf("  currently at %s\n", p.Current)
		fmt.Printf("  %.0f entries/s, running for %s\n", p.Rate(), p.Elapsed().Round(time.Second))
		return false, nil
	}
}
