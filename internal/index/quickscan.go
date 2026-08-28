package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// QuickScanUser finds files added, removed or renamed without reading the
// share.
//
// Two facts make this possible. A directory's modification time moves whenever
// an entry is added, removed or renamed inside it. And the index already knows
// every directory, so the tree does not need walking to find them.
//
// So a pass is one stat per indexed directory - no directory listings, no file
// stats, and no database work at all unless something moved. It costs the same
// whether the share holds a hundred files or ten million, because it scales
// with the number of directories rather than the number of files, and a stat
// of a directory whose metadata the kernel already has cached is close to free.
//
// What it cannot see is a file rewritten in place under the same name, which
// leaves its directory's timestamp untouched. That matters less than it sounds:
// a client reading a file fetches it from disk, so it always gets the current
// contents. What a stale index costs is a missing entry in a listing, and those
// are exactly what this catches. The full rescan remains the backstop.
func (s *Scanner) QuickScanUser(ctx context.Context, user store.User) (Stats, error) {
	start := time.Now()
	var stats Stats

	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		return stats, fmt.Errorf("open storage for %s: %w", user.Username, err)
	}

	// Every indexed directory, in one query. Nothing below touches the database
	// again unless a directory turns out to have changed.
	indexed, err := store.IndexedDirs(ctx, s.db, user.ID)
	if err != nil {
		return stats, fmt.Errorf("read indexed directories for %s: %w", user.Username, err)
	}
	if len(indexed) == 0 {
		s.log.Debug("no index yet for this account; a quick pass has nothing to compare",
			"user", user.Username)
		return stats, nil
	}

	changed, err := s.statDirs(ctx, st, indexed, &stats)
	if err != nil {
		return stats, err
	}

	changed = dedupe(changed)

	// Directories that still exist are settled before ones that have gone.
	//
	// A renamed directory appears in this list twice over: once as the old path,
	// which is now missing, and once as its parent, whose timestamp moved.
	// Handling the missing one first deletes the subtree from the index, and the
	// rename can then never be recognised - the entry it would have matched is
	// already gone. Settling the parent first lets the rename be seen, after
	// which the old path is no longer indexed and there is nothing to delete.
	present := make([]string, 0, len(changed))
	missing := make([]string, 0, len(changed))
	for _, dirPath := range changed {
		if _, err := st.Stat(dirPath); err == nil {
			present = append(present, dirPath)
			continue
		}
		missing = append(missing, dirPath)
	}
	// Within each group, deepest first, so a directory's children are settled
	// before the ETags above it are derived from them.
	byDepth := func(paths []string) {
		sort.Slice(paths, func(i, j int) bool { return depth(paths[i]) > depth(paths[j]) })
	}
	byDepth(present)
	byDepth(missing)

	for _, dirPath := range append(present, missing...) {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := s.refreshDirectory(ctx, st, user, dirPath, &stats); err != nil {
			return stats, err
		}
	}
	stats.Changed = int64(len(changed))

	stats.Duration = time.Since(start)
	// The worker count is reported because it is the knob that decides how long
	// this took, and the useful setting depends on the storage rather than on
	// anything Mirage can work out for itself.
	if stats.Changed > 0 {
		s.log.Info("quick scan complete",
			"user", user.Username, "directories_checked", stats.Dirs,
			"directories_changed", stats.Changed, "workers", s.scanWorkers(),
			"duration", stats.Duration)
	} else {
		s.log.Debug("quick scan complete, nothing changed",
			"user", user.Username, "directories_checked", stats.Dirs,
			"workers", s.scanWorkers(), "duration", stats.Duration)
	}
	return stats, nil
}

// statDirs reads every indexed directory's timestamp and returns the paths that
// need a closer look.
//
// The reads are issued concurrently. Each one is a disk seek on a share whose
// metadata does not fit in cache, and a seek is spent waiting rather than
// working, so a single-threaded pass leaves the drive idle most of the time.
// Concurrent reads let the drive queue and reorder them, and let an array use
// more than one spindle. This is the one place in the scanner where goroutines
// help: the work is waiting on IO, not on the CPU.
func (s *Scanner) statDirs(ctx context.Context, st *fsx.Storage,
	indexed map[string]store.DirState, stats *Stats) ([]string, error) {

	workers := min(s.scanWorkers(), len(indexed))

	type job struct {
		path string
		prev store.DirState
	}
	jobs := make(chan job)

	var (
		mu      sync.Mutex
		changed = make([]string, 0, 16)
	)
	report := func(dirPath string) {
		mu.Lock()
		changed = append(changed, dirPath)
		mu.Unlock()
	}

	// Its own context so that one worker's failure, or the caller's
	// cancellation, stops the others rather than leaving them to finish a pass
	// whose result is discarded.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var counted atomic.Int64
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				counted.Add(1)
				info, err := st.Stat(j.path)
				switch {
				case errors.Is(err, os.ErrNotExist):
					// Gone. Its parent's timestamp moved when it went, so the
					// parent is queued too and will prune it; queueing the
					// parent explicitly covers a filesystem that did not
					// update it.
					report(parentOfPath(j.path))
				case err != nil:
					// Unreadable now; the full rescan settles it.
				case !info.IsDir():
					report(parentOfPath(j.path))
				case j.prev.Changed(info.ModTime()):
					report(j.path)
				}
			}
		}()
	}

	var feedErr error
feed:
	for dirPath, prev := range indexed {
		select {
		case jobs <- job{path: dirPath, prev: prev}:
		case <-ctx.Done():
			feedErr = ctx.Err()
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	stats.Dirs += counted.Load()
	if feedErr != nil {
		return nil, feedErr
	}
	return changed, nil
}

// scanWorkers is how many directory timestamps to read at once.
//
// The default is deliberately well above the core count: these goroutines are
// blocked on the disk, not computing, so the useful number tracks how many
// requests the storage will accept at once rather than how many CPUs there are.
// It is capped so that a machine reporting a large core count does not queue
// thousands of seeks, which on a single spinning disk makes things worse.
func (s *Scanner) scanWorkers() int {
	if s.workers > 0 {
		return s.workers
	}
	return min(max(4*runtime.NumCPU(), 16), 64)
}

// refreshDirectory re-reads one directory's own entries, without descending
// into subdirectories that already exist.
//
// Existing subdirectories look after themselves: each has its own timestamp and
// is checked in its own right by the pass above. Only a subdirectory the index
// has never seen is scanned in full, since nothing else will find what is
// inside it.
func (s *Scanner) refreshDirectory(ctx context.Context, st *fsx.Storage, user store.User,
	dirPath string, stats *Stats) error {

	info, err := st.Stat(dirPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.updater.Removed(ctx, user, dirPath)
		}
		return nil //nolint:nilerr // unreadable now; the full rescan settles it
	}

	node, err := store.NodeByPath(ctx, s.db, user.ID, dirPath)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Not indexed at all, so there is no picture to refresh; scan it.
			return s.ScanPath(ctx, user, dirPath)
		}
		return err
	}

	entries, _, err := st.ReadDirReportingSkips(dirPath)
	if err != nil {
		return nil //nolint:nilerr // as above
	}

	existing, err := store.ChildNodes(ctx, s.db, node.ID)
	if err != nil {
		return err
	}
	known := make(map[string]store.Node, len(existing))
	for _, c := range existing {
		known[c.Name] = c
	}

	stamp := store.Stamp()
	digests := make([]ChildDigest, 0, len(entries))
	pending := make([]store.Node, 0, len(entries))
	unchanged := make([]int64, 0, len(entries))
	newSubdirs := make([]string, 0, 4)
	seen := make(map[string]bool, len(entries))
	var total int64

	for _, entry := range entries {
		name := entry.Name()
		childPath := fsx.Join(dirPath, name)
		seen[name] = true

		if entry.IsDir() {
			prev, ok := known[name]
			if !ok {
				// A directory that is new here may be one that moved. Carrying
				// its entry across takes the whole subtree with it, so the scan
				// that follows finds everything already in place.
				if info, err := entry.Info(); err == nil {
					if moved, err := s.detectDirRename(ctx, st, user, childPath, info); err != nil {
						return err
					} else if moved {
						stats.Moved++
						if carried, err := store.NodeByPath(ctx, s.db, user.ID, childPath); err == nil {
							digests = append(digests, ChildDigest{Name: name, ETag: carried.ETag})
							total += carried.Size
							continue
						}
					}
				}
				// Never seen before, so nothing else will look inside it.
				newSubdirs = append(newSubdirs, childPath)
				continue
			}
			digests = append(digests, ChildDigest{Name: name, ETag: prev.ETag})
			total += prev.Size
			continue
		}

		childInfo, err := entry.Info()
		if err != nil {
			seen[name] = false
			continue
		}
		etag := FileETag(childInfo.Size(), childInfo.ModTime())
		if _, ok := known[name]; !ok {
			// A name that is new here may be a file that moved. Detected before
			// the entries that vanished are deleted below, since the match is
			// against the index entry at the old path - and losing the file's
			// ID would make every client delete and re-download it.
			if moved, err := s.detectRename(ctx, st, user, childPath, childInfo); err != nil {
				return err
			} else if moved {
				stats.Moved++
			}
		}
		if prev, ok := known[name]; ok && !prev.IsDir && prev.ETag == etag {
			unchanged = append(unchanged, prev.ID)
			digests = append(digests, ChildDigest{Name: name, ETag: prev.ETag})
			total += prev.Size
			stats.Unchanged++
			continue
		}
		pending = append(pending, store.Node{
			UserID: user.ID, ParentID: node.ID, Path: childPath, Name: name,
			Size: childInfo.Size(), MTime: childInfo.ModTime(), ETag: etag,
			ContentType: contentType(name),
			Dev:         devOf(childInfo), Inode: inodeOf(childInfo),
		})
		digests = append(digests, ChildDigest{Name: name, ETag: etag})
		total += childInfo.Size()
		stats.Files++
	}

	// Anything indexed here and no longer on disk. Removed before the new
	// subdirectories are scanned, so a rename of a directory does not briefly
	// have both names present.
	var removed []string
	for name, c := range known {
		if !seen[name] {
			removed = append(removed, c.Path)
		}
	}

	unlock := indexLocks.lock(user.ID)
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, p := range removed {
			if err := store.DeleteNode(ctx, tx, user.ID, p); err != nil {
				return err
			}
		}
		for _, n := range pending {
			if _, err := store.UpsertNode(ctx, tx, n, stamp); err != nil {
				return err
			}
		}
		return store.TouchNodes(ctx, tx, unchanged, stamp)
	})
	unlock()
	if err != nil {
		return fmt.Errorf("refresh %s: %w", dirPath, err)
	}
	stats.Removed += int64(len(removed))

	// New subdirectories are scanned in full, which also settles their ETags.
	for _, sub := range newSubdirs {
		if err := s.ScanPath(ctx, user, sub); err != nil {
			return err
		}
		child, err := store.NodeByPath(ctx, s.db, user.ID, sub)
		if err != nil {
			return err
		}
		digests = append(digests, ChildDigest{Name: path.Base(sub), ETag: child.ETag})
		total += child.Size
		stats.Dirs++
	}

	unlock = indexLocks.lock(user.ID)
	defer unlock()
	if err := store.FinalizeDirNode(ctx, s.db, node.ID, DirETag(digests), total,
		info.ModTime(), stamp); err != nil {
		return err
	}
	// The change has to reach the root, or a client that skips a directory
	// whose ETag it already knows never looks inside this one.
	return propagate(ctx, s.db, user.ID, parentOfPath(dirPath))
}

func parentOfPath(p string) string {
	if fsx.IsRoot(p) {
		return fsx.RootPath
	}
	parent := path.Dir(p)
	if parent == "" || parent == "/" {
		return fsx.RootPath
	}
	return parent
}

func depth(p string) int {
	if fsx.IsRoot(p) {
		return 0
	}
	return 1 + strings.Count(p, "/")
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
