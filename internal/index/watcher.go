package index

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// defaultDebounce is how long changes are collected before being applied.
//
// A single save from an editor produces a burst of write events, and an
// unpacking archive produces thousands. Coalescing them means one index update
// and one notification per file instead of dozens.
const defaultDebounce = 400 * time.Millisecond

// Watcher keeps the index in step with the filesystem as changes happen.
//
// It is an optimisation, never the source of truth. Watches can be exhausted,
// events can be dropped under load, and nothing is delivered while the server
// is down. The periodic rescan is what makes the index correct; this is what
// makes it fast. Anything the watcher misses is caught by the next scan.
type Watcher struct {
	db      *store.DB
	storage *fsx.Manager
	scanner *Scanner
	updater *Updater
	log     *slog.Logger

	// maxWatches caps how many directories are watched. Zero derives it.
	maxWatches int
	debounce   time.Duration

	mu sync.Mutex
	// homes maps each watched user's home directory to their account, so an
	// event's absolute path can be resolved back to a user.
	homes map[string]store.User
}

// NewWatcher builds a Watcher.
func NewWatcher(db *store.DB, storage *fsx.Manager, scanner *Scanner, updater *Updater,
	log *slog.Logger, maxWatches int) *Watcher {
	return &Watcher{
		db: db, storage: storage, scanner: scanner, updater: updater, log: log,
		maxWatches: maxWatches,
		debounce:   defaultDebounce,
		homes:      make(map[string]store.User),
	}
}

// Run watches every enabled user's home until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer fsw.Close()

	users, err := w.db.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}

	budget := w.watchBudget()
	var watched, total int

	for _, u := range users {
		if u.Disabled {
			continue
		}
		home, err := filepath.Abs(u.Home)
		if err != nil {
			w.log.Warn("cannot watch home", "user", u.Username, "error", err)
			continue
		}
		w.mu.Lock()
		w.homes[home] = u
		w.mu.Unlock()

		dirs, err := store.CountDirs(ctx, w.db, u.ID)
		if err != nil {
			w.log.Warn("could not count directories", "user", u.Username, "error", err)
		}
		total += int(dirs)

		// Divided between accounts rather than given to whoever is scanned
		// first, so one large share does not leave another with nothing.
		share := budget / max(1, len(users))
		n, err := w.watchRecent(ctx, fsw, u, home, share)
		watched += n
		if err != nil {
			w.reportWatchFailure(u, err)
		}
	}

	switch {
	case watched == 0:
		w.log.Warn("no directories are being watched; changes made outside Mirage " +
			"will only be seen by the periodic rescan")
	case total > watched:
		// Partial coverage is stated plainly, along with what it means, because
		// the alternative is changes appearing promptly in one part of the tree
		// and not at all in another, which reads as intermittent failure.
		w.log.Info("watching the most recently changed directories; the rest are "+
			"covered by the periodic passes",
			"watched", watched, "directories", total,
			"kernel_limit", inotifyLimit(),
			"note", "watches are a fixed kernel resource, so they are spent where "+
				"changes actually happen rather than on archives")
	default:
		w.log.Info("watching for changes", "directories", watched, "users", len(w.homes))
	}

	return w.consume(ctx, fsw)
}

// watchBudget is how many directories may be watched in total.
//
// Derived from the kernel's own limit when not configured, leaving most of it
// for everything else on the machine. The default limit is a few thousand,
// which on a large share is a rounding error against the number of directories
// - so the point is not to cover the tree but to spend a small budget well.
func (w *Watcher) watchBudget() int {
	if w.maxWatches > 0 {
		return w.maxWatches
	}
	limit := inotifyLimit()
	if limit <= 0 {
		return defaultWatchBudget
	}
	if budget := limit / 2; budget > 0 {
		return budget
	}
	return defaultWatchBudget
}

// defaultWatchBudget is used where the kernel limit cannot be read.
const defaultWatchBudget = 4096

// watchRecent watches a user's most recently changed directories.
//
// Deliberately not a tree walk. Walking a share of three-quarters of a million
// directories to add a few thousand watches costs many minutes of disk and
// spends the budget on whichever directories sort first - which on an archive
// is precisely the ones that never change. The index already records when each
// directory last changed, so the ones worth watching can be asked for directly.
func (w *Watcher) watchRecent(ctx context.Context, fsw *fsnotify.Watcher,
	u store.User, home string, budget int) (int, error) {

	dirs, err := store.RecentDirs(ctx, w.db, u.ID, budget)
	if err != nil {
		return 0, err
	}
	if len(dirs) == 0 {
		// No index yet, so there is nothing to choose from. The first scan
		// builds one and the next start can be selective.
		return 0, nil
	}

	var added int
	var firstErr error
	for _, rel := range dirs {
		path := home
		if !fsx.IsRoot(rel) {
			path = filepath.Join(home, filepath.FromSlash(rel))
		}
		if err := fsw.Add(path); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// Out of watches: nothing further will succeed either, so stop
			// rather than failing once per remaining directory.
			if isWatchLimit(err) {
				break
			}
			continue
		}
		added++
	}
	return added, firstErr
}

// isWatchLimit reports whether an error means the kernel is out of watches.
func isWatchLimit(err error) bool {
	return errors.Is(err, syscall.ENOSPC) ||
		strings.Contains(err.Error(), "no space left on device")
}

// watchTree adds a watch to dir and every directory beneath it, returning how
// many were added.
//
// inotify does not recurse, so each directory needs its own watch. Failures are
// collected rather than fatal: watching most of a tree is better than none.
func (w *Watcher) watchTree(fsw *fsnotify.Watcher, dir string) (added, seen int, err error) {
	var firstErr error

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory is skipped, exactly as the scanner
			// skips it, rather than aborting the walk.
			return nil //nolint:nilerr // deliberate
		}
		if !d.IsDir() {
			return nil
		}
		if fsx.IsInternal(d.Name()) {
			return filepath.SkipDir
		}
		seen++
		if err := fsw.Add(path); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		added++
		return nil
	})
	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
	}
	return added, seen, firstErr
}

// inotifyLimit reports the kernel's per-user watch limit, or 0 where it cannot
// be read. Quoting it back turns "no space left on device" into a number the
// operator can compare against their tree.
func inotifyLimit() int {
	raw, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

// reportWatchFailure explains a failed watch in terms an operator can act on.
func (w *Watcher) reportWatchFailure(u store.User, err error) {
	// Running out of inotify watches is the common failure on a NAS with a
	// large tree, and the message is otherwise a bare "no space left on
	// device", which points at the disk rather than at the real limit.
	if errors.Is(err, os.ErrNotExist) {
		w.log.Warn("cannot watch home: it does not exist", "user", u.Username, "home", u.Home)
		return
	}
	if isWatchLimit(err) || errors.Is(err, fsnotify.ErrEventOverflow) {
		w.log.Info("the kernel would allow no more watches; the rest of this tree is "+
			"covered by the periodic passes",
			"user", u.Username, "kernel_limit", inotifyLimit(),
			"hint", "raise fs.inotify.max_user_watches to watch more of it, "+
				"or set storage.max_watches to choose the budget explicitly")
		return
	}
	w.log.Warn("could not watch part of this tree; the periodic rescan still covers it",
		"user", u.Username, "error", err)
}

// consume reads events until ctx is done, applying them in debounced batches.
func (w *Watcher) consume(ctx context.Context, fsw *fsnotify.Watcher) error {
	pending := make(map[string]struct{})
	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	timerRunning := false

	for {
		select {
		case <-ctx.Done():
			return nil

		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			// Attribute-only changes do not alter content, size or mtime, so
			// they cannot change any ETag and are not worth a round trip.
			if event.Op == fsnotify.Chmod {
				continue
			}
			if fsx.IsInternal(filepath.Base(event.Name)) || w.isInternalPath(event.Name) {
				continue
			}
			// A new directory needs its own watch, and it may already contain
			// files that arrived before the watch was added.
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if _, _, err := w.watchTree(fsw, event.Name); err != nil {
						w.log.Debug("could not watch new directory", "path", event.Name, "error", err)
					}
				}
			}
			pending[event.Name] = struct{}{}
			if !timerRunning {
				timer.Reset(w.debounce)
				timerRunning = true
			}

		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				// The kernel queue overflowed, so an unknown set of changes was
				// lost. A full rescan is the only way back to a known state.
				w.log.Warn("filesystem event queue overflowed; forcing a rescan")
				if err := w.scanner.ScanAll(ctx, "filesystem event queue overflowed"); err != nil && !errors.Is(err, context.Canceled) {
					w.log.Error("recovery rescan failed", "error", err)
				}
				continue
			}
			w.log.Warn("filesystem watch error", "error", err)

		case <-timer.C:
			timerRunning = false
			batch := pending
			pending = make(map[string]struct{})
			w.applyBatch(ctx, batch)
		}
	}
}

// isInternalPath reports whether a path lies inside Mirage's own scratch area.
func (w *Watcher) isInternalPath(path string) bool {
	return strings.Contains(path, string(filepath.Separator)+fsx.UploadDir+string(filepath.Separator)) ||
		strings.HasSuffix(path, string(filepath.Separator)+fsx.UploadDir)
}

// applyBatch reconciles a debounced batch of changed paths.
//
// Order matters, and in one specific way. A rename arrives as two events: the
// old path disappearing and the new one appearing, both in the same batch.
// Rename detection recognises the move by finding the old entry still in the
// index and matching its inode - so if the disappearance were handled first,
// the entry would already be gone, the new path would be indexed as a new file
// with a new ID, and every client would delete and re-download it.
//
// So paths that currently exist are settled before paths that do not, and the
// move is seen as a move. Sorting within each group is for determinism and to
// put a parent directory ahead of its children.
func (w *Watcher) applyBatch(ctx context.Context, batch map[string]struct{}) {
	present := make([]string, 0, len(batch))
	missing := make([]string, 0, len(batch))
	for path := range batch {
		if _, err := os.Lstat(path); err == nil {
			present = append(present, path)
			continue
		}
		missing = append(missing, path)
	}
	sort.Strings(present)
	sort.Strings(missing)

	for _, path := range append(present, missing...) {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := w.apply(ctx, path); err != nil {
			w.log.Warn("could not apply a filesystem change", "path", path, "error", err)
		}
	}
}

// apply reconciles one path against what is on disk.
//
// It reads the current state rather than trusting the event: by the time a
// batch is processed a file may have been created and deleted again, and the
// filesystem is the authority on which.
func (w *Watcher) apply(ctx context.Context, absPath string) error {
	user, rel, ok := w.resolve(absPath)
	if !ok {
		return nil
	}
	if fsx.IsRoot(rel) {
		return nil // the home directory itself
	}

	st, err := w.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		return err
	}

	info, err := st.Stat(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Gone, or moved away. Either way it is no longer here; if it
			// moved, the destination arrives as its own event and reclaims the
			// file ID through rename detection.
			return w.updater.Removed(ctx, user, rel)
		}
		return err
	}

	if info.IsDir() {
		// A directory may have appeared with contents already inside it, so the
		// subtree is scanned rather than merely noted.
		return w.scanner.ScanPath(ctx, user, rel)
	}
	if !info.Mode().IsRegular() {
		return nil
	}

	if _, err := w.scanner.detectRename(ctx, st, user, rel, info); err != nil {
		return err
	}
	_, err = w.updater.FileWritten(ctx, user, rel, info.Size(), info.ModTime())
	return err
}

// resolve maps an absolute filesystem path back to a user and a path relative
// to their home.
func (w *Watcher) resolve(absPath string) (store.User, string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for home, user := range w.homes {
		if absPath == home {
			return user, fsx.RootPath, true
		}
		prefix := strings.TrimSuffix(home, string(filepath.Separator)) + string(filepath.Separator)
		if !strings.HasPrefix(absPath, prefix) {
			continue
		}
		rel, err := fsx.CleanPath(filepath.ToSlash(strings.TrimPrefix(absPath, prefix)))
		if err != nil {
			return store.User{}, "", false
		}
		return user, rel, true
	}
	return store.User{}, "", false
}
