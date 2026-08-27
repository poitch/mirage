package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// cmdLs shows what Mirage has indexed, as opposed to what is on disk.
//
// When a client is missing a folder, the question is which of the two is
// short: the filesystem, the index, or the client. `ls` answers the middle one
// directly, and alongside it reports what the filesystem holds, so a
// disagreement between them is visible in one place.
func cmdLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	configPath, verbose := configFlags(fs)
	username := fs.String("user", "", "account to inspect (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("-user is required")
	}
	target := "."
	if rest := fs.Args(); len(rest) > 0 {
		target = rest[0]
	}

	log := newLogger(*verbose)
	cfg, db, err := setup(*configPath, log)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	user, err := db.UserByName(ctx, *username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account %q", *username)
	} else if err != nil {
		return err
	}

	clean, err := fsx.CleanPath(target)
	if err != nil {
		return fmt.Errorf("invalid path %q: %w", target, err)
	}

	node, err := store.NodeByPath(ctx, db, user.ID, clean)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Printf("%s is not indexed for %s.\n", clean, user.Username)
		fmt.Println("If it exists on disk, the scan has not reached it yet; run `mirage scan`.")
		return nil
	} else if err != nil {
		return err
	}

	children, err := store.ChildNodes(ctx, db, node.ID)
	if err != nil {
		return err
	}

	fmt.Printf("%s  %s\n", user.Username, node.Path)
	fmt.Printf("  home   %s\n", user.Home)
	fmt.Printf("  etag   %s\n", node.ETag)
	fmt.Printf("  size   %s across %d entries\n\n", formatBytes(node.Size), len(children))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tSIZE\tMODIFIED\tFILEID")
	for _, c := range children {
		kind := "file"
		if c.IsDir {
			kind = "dir"
		}
		modified := c.MTime.Format(time.RFC3339)
		// A timestamp at or before the epoch means the entry was created but
		// never finalised, which is what a mid-scan directory looks like.
		if c.MTime.Unix() <= 0 {
			modified += "  (never finalised)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", c.Name, kind, formatBytes(c.Size), modified, c.ID)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// What the filesystem holds, for comparison. A name here but not above
	// means the index is behind; the reverse means it was deleted out of band.
	// Already validated when the config loaded, so this cannot fail here.
	excluder, _ := fsx.NewExcluder(cfg.Storage.Exclude)
	storage := fsx.NewManager(cfg.Storage.FileMode.Perm(), cfg.Storage.DirMode.Perm(), excluder)
	defer storage.Close()
	st, err := storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		return nil
	}
	entries, skippedLinks, err := st.ReadDirReportingSkips(clean)
	if err != nil {
		fmt.Printf("\nCould not read %s on disk: %v\n", clean, err)
		return nil
	}

	indexed := make(map[string]bool, len(children))
	for _, c := range children {
		indexed[c.Name] = true
	}
	var missing []string
	for _, e := range entries {
		if !indexed[e.Name()] {
			missing = append(missing, e.Name())
		}
	}
	if len(missing) > 0 {
		fmt.Printf("\nOn disk but not indexed (%d): %v\n", len(missing), missing)
		fmt.Println("Run `mirage scan` to pick them up.")
	}
	if len(skippedLinks) > 0 {
		fmt.Printf("\nSymbolic links, which are never followed (%d): %v\n", len(skippedLinks), skippedLinks)
		fmt.Println("Bind-mount the target into the account's directory if it should sync.")
	}
	return nil
}
