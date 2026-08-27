package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// QuickScanUser finds files added, removed or renamed without stat'ing every
// file in the share.
//
// A full pass over a few million files takes long enough that running it often
// is unreasonable, yet a file dropped over SMB should appear on clients within
// minutes. Where the kernel cannot watch every directory - a few hundred
// thousand of them exceeds any sane inotify limit - this is what closes that
// gap.
//
// It works from one fact: a directory's modification time changes when an entry
// is added, removed or renamed inside it. So the tree can be walked comparing
// only directory timestamps, and the expensive per-file work done solely for
// the directories that actually moved.
//
// What it cannot see is a file rewritten in place under the same name, which
// leaves its directory's timestamp untouched. The full rescan remains the
// backstop for those, and the filesystem watcher catches them live wherever it
// has a watch.
func (s *Scanner) QuickScanUser(ctx context.Context, user store.User) (Stats, error) {
	start := time.Now()
	var stats Stats

	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		return stats, fmt.Errorf("open storage for %s: %w", user.Username, err)
	}

	// Every indexed directory, in one query. The walk below then compares
	// timestamps without going near the database again.
	indexed, err := store.IndexedDirs(ctx, s.db, user.ID)
	if err != nil {
		return stats, fmt.Errorf("read indexed directories for %s: %w", user.Username, err)
	}
	if len(indexed) == 0 {
		// Nothing to compare against; a full pass has to establish the picture.
		s.log.Info("no index yet for this account; a quick pass has nothing to compare",
			"user", user.Username)
		return stats, nil
	}

	changed, err := s.findChangedDirs(ctx, st, user, indexed, &stats)
	if err != nil {
		return stats, err
	}

	// Deepest first, so a directory's children are settled before its own ETag
	// is derived from them.
	sort.Slice(changed, func(i, j int) bool {
		return depth(changed[i]) > depth(changed[j])
	})

	for _, dirPath := range changed {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := s.rescanOneDirectory(ctx, st, user, dirPath, &stats); err != nil {
			return stats, err
		}
	}
	stats.Changed = int64(len(changed))

	stats.Duration = time.Since(start)
	s.log.Info("quick scan complete",
		"user", user.Username, "directories_checked", stats.Dirs,
		"directories_changed", stats.Changed, "duration", stats.Duration)
	return stats, nil
}

// findChangedDirs walks the tree comparing directory timestamps, returning the
// paths whose contents may have changed.
//
// A directory absent from the index is new, and one whose timestamp has moved
// had something added, removed or renamed in it. Neither requires reading a
// single file.
func (s *Scanner) findChangedDirs(ctx context.Context, st *fsx.Storage, user store.User,
	indexed map[string]store.DirState, stats *Stats) ([]string, error) {

	var changed []string
	seen := make(map[string]bool, len(indexed))

	var walk func(dirPath string) error
	walk = func(dirPath string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		seen[dirPath] = true
		stats.Dirs++

		info, err := st.Stat(dirPath)
		if err != nil {
			// Gone, or unreadable. Either way its parent's timestamp moved, so
			// the parent is already queued and will settle it.
			return nil //nolint:nilerr // deliberate
		}

		prev, known := indexed[dirPath]
		if !known || prev.Changed(info.ModTime()) {
			changed = append(changed, dirPath)
		}

		// Subdirectories are visited regardless: a change further down does not
		// touch this directory's timestamp.
		entries, _, err := st.ReadDirReportingSkips(dirPath)
		if err != nil {
			return nil //nolint:nilerr // unreadable, as above
		}
		for _, e := range entries {
			if e.IsDir() {
				if err := walk(fsx.Join(dirPath, e.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(fsx.RootPath); err != nil {
		return nil, err
	}

	// A directory in the index that no longer exists on disk. Its parent's
	// timestamp moved when it went, so the parent is already in the list and
	// will prune it - but a parent that was itself skipped as unchanged, which
	// some filesystems allow, would leave it stranded.
	for dirPath := range indexed {
		if !seen[dirPath] {
			parent := path.Dir(dirPath)
			if parent == "" || parent == "/" {
				parent = fsx.RootPath
			}
			changed = append(changed, parent)
		}
	}
	return dedupe(changed), nil
}

// rescanOneDirectory re-reads a single directory and propagates the result
// upwards, leaving the rest of the tree alone.
func (s *Scanner) rescanOneDirectory(ctx context.Context, st *fsx.Storage, user store.User,
	dirPath string, _ *Stats) error {

	info, err := st.Stat(dirPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The directory went away; removing it also removes its subtree.
			return s.updater.Removed(ctx, user, dirPath)
		}
		return nil //nolint:nilerr // unreadable right now; the full scan will settle it
	}
	if !info.IsDir() {
		return nil
	}
	// ScanPath re-reads this directory and everything under it, then refreshes
	// the ETags above it. For a directory whose contents changed that is
	// exactly the work needed, and it is bounded by the subtree rather than
	// the share.
	return s.ScanPath(ctx, user, dirPath)
}

func depth(p string) int {
	if fsx.IsRoot(p) {
		return 0
	}
	n := 1
	for i := range len(p) {
		if p[i] == '/' {
			n++
		}
	}
	return n
}

func dedupe(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := paths[:0]
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
