package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// Scanner rebuilds the index from what is actually on disk.
//
// The filesystem is authoritative: whatever a scan finds wins over what the
// index believed. That is what lets files arrive over SMB or DSM File Station
// and still reach sync clients.
type Scanner struct {
	db       *store.DB
	storage  *fsx.Manager
	log      *slog.Logger
	notifier Notifier
	updater  *Updater
	// workers caps how many directory timestamps a quick pass reads at once.
	// Zero derives a figure; see Scanner.scanWorkers.
	workers int
}

// SetNotifier attaches a change notifier. Passing nil disables notification.
func (s *Scanner) SetNotifier(n Notifier) { s.notifier = n }

// SetWorkers sets how many directory timestamps a quick pass reads at once.
// Zero restores the derived default.
func (s *Scanner) SetWorkers(n int) { s.workers = max(n, 0) }

// NewScanner builds a Scanner.
func NewScanner(db *store.DB, storage *fsx.Manager, log *slog.Logger) *Scanner {
	u := NewUpdater(db)
	u.SetStorage(storage)
	return &Scanner{db: db, storage: storage, log: log, updater: u}
}

// Stats summarises one scan.
type Stats struct {
	Files   int64
	Dirs    int64
	Bytes   int64
	Moved   int64 // entries recognised as renamed rather than new
	Removed int64 // index entries dropped because they are gone from disk
	Skipped int64 // directories that could not be read and were left untouched
	// SkippedLinks counts symbolic links that were not followed. On a NAS these
	// are usually links into shared folders outside the account's directory.
	SkippedLinks int64
	// Resumed counts directories skipped because an interrupted scan had
	// already finished reading them under the same generation.
	Resumed int64
	// Unchanged counts files whose metadata matched the index, and which were
	// therefore marked as seen rather than rewritten.
	Unchanged int64
	// Changed counts directories a quick pass found to have moved and therefore
	// re-read. On a settled share this is zero and the pass does no writes.
	Changed  int64
	Duration time.Duration
}

// ScanUser walks a user's home directory and brings their index up to date.
func (s *Scanner) ScanUser(ctx context.Context, user store.User) (Stats, error) {
	return s.scanUser(ctx, user)
}

func (s *Scanner) scanUser(ctx context.Context, user store.User) (Stats, error) {
	start := time.Now()
	var stats Stats

	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		return stats, fmt.Errorf("open storage for %s: %w", user.Username, err)
	}

	// Everything this scan touches is stamped with one marker, and whatever
	// still carries an older one at the end is gone from disk. Sweeping at the
	// end rather than per directory is what lets a moved file keep its ID: the
	// entry at its old path is still there when the new one is reached.
	// A scan that was interrupted is picked up rather than started again. It
	// keeps the generation marker it was using, so every directory it already
	// finished is recognisable and can be skipped whole - which on a large
	// share is the difference between resuming and repeating an hour of work.
	stamp := store.Stamp()
	resuming := false
	detectRenames := false

	if prev, ok, err := ScanProgress(ctx, s.db); err != nil {
		return stats, err
	} else if ok && prev.Resumable(user.Username) {
		stamp, resuming, detectRenames = prev.Stamp, true, prev.DetectRenames
		s.log.Info("resuming the interrupted scan",
			"user", user.Username, "already_indexed", prev.Files+prev.Dirs)
	} else {
		// A file can only be recognised as moved if the index already knows it
		// somewhere else, so on an empty index every lookup is guaranteed to
		// miss. Skipping them matters precisely when it is most painful: the
		// first scan of a large share, where every name is new.
		indexed, err := store.CountNodes(ctx, s.db, user.ID)
		if err != nil {
			return stats, fmt.Errorf("count indexed entries for %s: %w", user.Username, err)
		}
		detectRenames = indexed > 0
		if !detectRenames {
			s.log.Info("first scan for this account; nothing can have moved yet",
				"user", user.Username)
		}
	}

	// The root ETag summarises the whole tree, so comparing it before and after
	// is an exact test of whether this scan found anything. That matters
	// because a scan runs on a timer: notifying unconditionally would wake
	// every client every interval for nothing.
	before := s.rootETag(ctx, user.ID)

	progress := s.newProgress(ctx, user.Username, stamp, detectRenames)
	run := &scanRun{
		storage: st, user: user, stamp: stamp, stats: &stats, progress: progress,
		detectRenames: detectRenames, resuming: resuming,
	}
	if _, _, err := s.scanDir(ctx, run, fsx.RootPath, 0); err != nil {
		progress.finish(ctx, &stats, err)
		return stats, err
	}

	removed, err := store.SweepUnscanned(ctx, s.db, user.ID, stamp)
	if err != nil {
		err = fmt.Errorf("prune index for %s: %w", user.Username, err)
		progress.finish(ctx, &stats, err)
		return stats, err
	}
	progress.finish(ctx, &stats, nil)
	stats.Removed = removed

	if after := s.rootETag(ctx, user.ID); after != before && s.notifier != nil {
		s.notifier.FileChanged(user.ID, nil)
	}

	stats.Duration = time.Since(start)
	s.log.Info("scan complete",
		"user", user.Username, "files", stats.Files, "dirs", stats.Dirs,
		"bytes", stats.Bytes, "moved", stats.Moved, "removed", stats.Removed,
		"skipped", stats.Skipped, "skipped_links", stats.SkippedLinks,
		"resumed", stats.Resumed, "unchanged", stats.Unchanged, "duration", stats.Duration)
	return stats, nil
}

// scanRun carries the settings that apply to a whole scan, so they are not
// threaded through the recursion as a growing list of positional flags.
type scanRun struct {
	storage  *fsx.Storage
	user     store.User
	stamp    int64
	stats    *Stats
	progress *progressReporter

	// detectRenames looks up whether a newly-seen file is one that moved.
	detectRenames bool
	// resuming skips subtrees an interrupted scan already finished.
	resuming bool
}

// scanDir indexes one directory and everything beneath it, returning the
// directory's derived ETag and its recursive size.
func (s *Scanner) scanDir(ctx context.Context, run *scanRun,
	dirPath string, parentID int64) (etag string, size int64, err error) {

	st, user, stamp := run.storage, run.user, run.stamp
	stats, progress := run.stats, run.progress

	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if progress != nil {
		progress.update(ctx, stats, dirPath)
	}

	// A directory already finished under this generation was completed by the
	// interrupted pass, so the whole subtree beneath it can be skipped.
	if run.resuming {
		if prev, err := store.NodeByPath(ctx, s.db, user.ID, dirPath); err == nil &&
			prev.IsDir && prev.Complete && prev.ScannedAt == stamp {
			// Everything under a skipped subtree is re-stamped as seen. In
			// principle finishing a directory already stamped its contents, so
			// this is redundant - but the end-of-scan sweep deletes whatever
			// carries an older mark, and if that invariant were ever wrong the
			// result would be entries disappearing from the index and clients
			// deleting the files. Not worth resting on.
			if err := store.MarkSubtreeScanned(ctx, s.db, user.ID, dirPath, stamp); err != nil {
				return "", 0, fmt.Errorf("preserve resumed subtree %s: %w", dirPath, err)
			}
			stats.Resumed++
			return prev.ETag, prev.Size, nil
		}
	}

	info, err := st.Stat(dirPath)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", dirPath, err)
	}

	name := path.Base(dirPath)
	if fsx.IsRoot(dirPath) {
		name = ""
	}

	// Checked before the row is created, since creating it would make the
	// directory look like one that had always been here.
	if run.detectRenames && !fsx.IsRoot(dirPath) {
		if moved, err := s.detectDirRename(ctx, st, user, dirPath, info); err != nil {
			return "", 0, err
		} else if moved {
			stats.Moved++
		}
	}

	// Seeded from the directory's own metadata so that a client reading this
	// row before the scan reaches the end of it sees something sensible.
	dirID, err := store.EnsureDirNode(ctx, s.db, user.ID, parentID, dirPath, name,
		info.ModTime(), FileETag(0, info.ModTime()), devOf(info), inodeOf(info), stamp)
	if err != nil {
		return "", 0, fmt.Errorf("index directory %s: %w", dirPath, err)
	}

	entries, skippedLinks, err := st.ReadDirReportingSkips(dirPath)
	if err != nil {
		// Critically, this does not fall through to the delete step below. A
		// directory that cannot be read right now is not an empty directory:
		// treating a transient permission error or a mount that dropped out as
		// "everything here is gone" would propagate deletions to every client
		// and destroy the user's files. Leave the subtree exactly as indexed
		// and let the next scan pick it up.
		s.log.Warn("skipping unreadable directory; its indexed contents are left untouched",
			"user", user.Username, "path", dirPath, "error", err)
		stats.Skipped++
		// The subtree must be stamped as seen, or the end-of-scan sweep would
		// delete every entry under it - and clients would then delete the real
		// files. The scan does not know what is in here; that is precisely why
		// it must not claim the contents are gone.
		if err := store.MarkSubtreeScanned(ctx, s.db, user.ID, dirPath, stamp); err != nil {
			return "", 0, fmt.Errorf("preserve unreadable subtree %s: %w", dirPath, err)
		}
		existing, lookupErr := store.NodeByPath(ctx, s.db, user.ID, dirPath)
		if lookupErr != nil {
			return "", 0, nil
		}
		return existing.ETag, existing.Size, nil
	}

	if len(skippedLinks) > 0 {
		// Logged at every scan rather than once, because the consequence -
		// a folder that is on the NAS but never appears on any client - is
		// otherwise invisible and impossible to guess at.
		s.log.Warn("not syncing symbolic links; a link may point outside the account's directory",
			"user", user.Username, "path", dirPath, "links", strings.Join(skippedLinks, ", "),
			"hint", "map the link target as its own account if it should sync")
		stats.SkippedLinks += int64(len(skippedLinks))
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	// The directory's existing entries, fetched once. Rename detection only
	// concerns names that are new here, and asking the database per file
	// instead would add a query for every file in the tree.
	existing, err := store.ChildNodes(ctx, s.db, dirID)
	if err != nil {
		return "", 0, fmt.Errorf("read indexed children of %s: %w", dirPath, err)
	}
	known := make(map[string]store.Node, len(existing))
	for _, c := range existing {
		known[c.Name] = c
	}

	digests := make([]ChildDigest, 0, len(entries))
	var total int64

	// Files are collected first and written together below. Reading and
	// writing are separated so that no filesystem I/O happens while the write
	// transaction is open, which would hold the database's single writer lock
	// across a disk seek.
	type pendingFile struct {
		node store.Node
		etag string
	}
	pending := make([]pendingFile, 0, len(entries))
	// Files whose metadata is unchanged since the last scan. They need no
	// rewrite, only their generation mark moved forward so the end-of-scan
	// sweep does not take them for deleted.
	unchanged := make([]int64, 0, len(entries))

	for _, entry := range entries {
		childName := entry.Name()
		childPath := fsx.Join(dirPath, childName)

		if entry.IsDir() {
			childETag, childSize, err := s.scanDir(ctx, run, childPath, dirID)
			if err != nil {
				return "", 0, err
			}
			digests = append(digests, ChildDigest{Name: childName, ETag: childETag})
			total += childSize
			stats.Dirs++
			continue
		}

		// Reported from inside the loop, not only once per directory: a
		// directory can hold tens of thousands of files, and updating only on
		// entry leaves a scan that is working steadily looking stopped.
		if progress != nil {
			progress.update(ctx, stats, childPath)
		}

		childInfo, err := entry.Info()
		if err != nil {
			// The file vanished between listing and stat. Drop it from this
			// pass rather than indexing something we could not measure.
			s.log.Debug("entry disappeared during scan", "path", childPath, "error", err)
			continue
		}

		childETag := FileETag(childInfo.Size(), childInfo.ModTime())

		// An entry whose recomputed ETag matches the stored one is unchanged,
		// so rewriting the row would store the values already there - and on a
		// rescan of an unchanged tree that is every row in the database.
		//
		// The comparison is against the ETag rather than against size and
		// modification time directly. The stored timestamp is only accurate to
		// the second, while the ETag is derived at nanosecond precision, so
		// comparing timestamps would miss a file rewritten twice within one
		// second at the same length. The ETag is computed here regardless.
		if prev, ok := known[childName]; ok && !prev.IsDir && prev.ETag == childETag {
			unchanged = append(unchanged, prev.ID)
			digests = append(digests, ChildDigest{Name: childName, ETag: prev.ETag})
			total += childInfo.Size()
			stats.Files++
			stats.Bytes += childInfo.Size()
			stats.Unchanged++
			continue
		}

		// Only a name that is new here can be a file that moved.
		if _, seen := known[childName]; run.detectRenames && !seen {
			if moved, err := s.detectRename(ctx, st, user, childPath, childInfo); err != nil {
				return "", 0, err
			} else if moved {
				stats.Moved++
			}
		}

		pending = append(pending, pendingFile{
			node: store.Node{
				UserID:      user.ID,
				ParentID:    dirID,
				Path:        childPath,
				Name:        childName,
				IsDir:       false,
				Size:        childInfo.Size(),
				MTime:       childInfo.ModTime(),
				ETag:        childETag,
				ContentType: contentType(childName),
				Dev:         devOf(childInfo),
				Inode:       inodeOf(childInfo),
			},
			etag: childETag,
		})

		digests = append(digests, ChildDigest{Name: childName, ETag: childETag})
		total += childInfo.Size()
		stats.Files++
		stats.Bytes += childInfo.Size()
	}

	// One transaction for the whole directory. Committing per file makes the
	// database the dominant cost of a scan; a directory at a time is a large
	// enough batch to amortise that without holding the write lock for long.
	if len(pending) > 0 || len(unchanged) > 0 {
		err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			for _, pf := range pending {
				if _, err := store.UpsertNode(ctx, tx, pf.node, stamp); err != nil {
					return fmt.Errorf("index file %s: %w", pf.node.Path, err)
				}
			}
			// Everything untouched moves forward in one statement rather than
			// one per file, which is what makes an unchanged rescan cheap.
			return store.TouchNodes(ctx, tx, unchanged, stamp)
		})
		if err != nil {
			return "", 0, err
		}
	}

	// Held only for this directory's mutation, not across the recursion above:
	// a full scan of a large tree must not block uploads for its duration.
	unlock := indexLocks.lock(user.ID)
	defer unlock()

	etag = DirETag(digests)
	if err := store.FinalizeDirNode(ctx, s.db, dirID, etag, total, info.ModTime(), stamp); err != nil {
		return "", 0, fmt.Errorf("finalize %s: %w", dirPath, err)
	}
	return etag, total, nil
}

// ScanPath reindexes one subtree and refreshes the directories above it.
//
// It exists for operations that create many entries at once - a COPY of a
// directory tree - where synthesising an index entry per file would duplicate
// the scanner badly.
func (s *Scanner) ScanPath(ctx context.Context, user store.User, target string) error {
	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		return fmt.Errorf("open storage for %s: %w", user.Username, err)
	}
	info, err := st.Stat(target)
	if err != nil {
		return fmt.Errorf("stat %s: %w", target, err)
	}

	parentPath := parentDir(target)
	var parentID int64
	if !fsx.IsRoot(target) {
		parent, err := store.NodeByPath(ctx, s.db, user.ID, parentPath)
		if err != nil {
			return fmt.Errorf("parent of %s is not indexed: %w", target, err)
		}
		parentID = parent.ID
	}

	stamp := store.Stamp()
	var stats Stats
	if info.IsDir() {
		run := &scanRun{storage: st, user: user, stamp: stamp, stats: &stats, detectRenames: true}
		if _, _, err := s.scanDir(ctx, run, target, parentID); err != nil {
			return err
		}
		// Scoped sweep: only entries under this subtree, since the rest of the
		// tree was never visited and must not be judged missing.
		if err := s.sweepSubtree(ctx, user, target, stamp); err != nil {
			return err
		}
	} else {
		unlock := indexLocks.lock(user.ID)
		name := path.Base(target)
		_, err := store.UpsertNode(ctx, s.db, store.Node{
			UserID:      user.ID,
			ParentID:    parentID,
			Path:        target,
			Name:        name,
			IsDir:       false,
			Size:        info.Size(),
			MTime:       info.ModTime(),
			ETag:        FileETag(info.Size(), info.ModTime()),
			ContentType: contentType(name),
			Dev:         devOf(info),
			Inode:       inodeOf(info),
		}, stamp)
		unlock()
		if err != nil {
			return fmt.Errorf("index %s: %w", target, err)
		}
	}

	unlock := indexLocks.lock(user.ID)
	defer unlock()
	return propagate(ctx, s.db, user.ID, parentPath)
}

// rootETag returns a user's root ETag, or "" if they have not been scanned.
func (s *Scanner) rootETag(ctx context.Context, userID int64) string {
	n, err := store.NodeByPath(ctx, s.db, userID, fsx.RootPath)
	if err != nil {
		return ""
	}
	return n.ETag
}

// sweepSubtree drops index entries under target that this pass did not touch.
func (s *Scanner) sweepSubtree(ctx context.Context, user store.User, target string, stamp int64) error {
	unlock := indexLocks.lock(user.ID)
	defer unlock()
	lo, hi, ok := store.PrefixRange(target + "/")
	if !ok {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM nodes
		WHERE user_id = ? AND scanned_at < ? AND path >= ? AND path < ?`,
		user.ID, stamp, lo, hi)
	return err
}

// detectDirRename carries a directory's index entry across a rename, along with
// every path beneath it.
//
// Without this a renamed directory is indexed as a new one and the old is
// swept, so a client sees a folder deleted and another created - and for a
// large folder that means discarding and re-fetching all of it. Because the
// move rewrites the paths of everything underneath, the subtree that follows
// also costs nothing: the scan finds it already where it expects.
func (s *Scanner) detectDirRename(ctx context.Context, st *fsx.Storage, user store.User,
	newPath string, info fs.FileInfo) (bool, error) {

	if _, err := store.NodeByPath(ctx, s.db, user.ID, newPath); err == nil {
		return false, nil // already indexed here; nothing moved
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}

	dev, inode, ok := fsx.Identity(info)
	if !ok || inode == 0 {
		return false, nil
	}
	previous, err := store.NodeByInode(ctx, s.db, user.ID, dev, inode, true)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if previous.Path == newPath || fsx.IsRoot(previous.Path) {
		return false, nil
	}
	// If the old path still exists this is not a move. A directory cannot be
	// hard-linked, but a filesystem that recycles inode numbers could otherwise
	// hand one directory another's identity.
	if _, err := st.Stat(previous.Path); err == nil {
		return false, nil
	}

	unlock := indexLocks.lock(user.ID)
	defer unlock()
	parent, err := store.NodeByPath(ctx, s.db, user.ID, parentDir(newPath))
	if err != nil {
		return false, nil //nolint:nilerr // parent not indexed yet; treat as new
	}
	if err := store.MoveNode(ctx, s.db, user.ID, previous.Path, newPath, parent.ID, path.Base(newPath)); err != nil {
		return false, fmt.Errorf("carry directory across rename: %w", err)
	}
	s.log.Debug("recognised a directory rename",
		"user", user.Username, "from", previous.Path, "to", newPath, "fileid", previous.ID)
	return true, nil
}

// detectRename checks whether a file that is new at this path is really one
// that moved, and if so carries its index entry - and its file ID - across.
//
// Clients use the file ID to tell a rename from a delete plus a create. Without
// this, renaming a large folder over SMB would make every client delete and
// re-download all of it.
func (s *Scanner) detectRename(ctx context.Context, st *fsx.Storage, user store.User,
	newPath string, info fs.FileInfo) (bool, error) {

	// Callers establish that the path is not already indexed before asking, so
	// that is deliberately not re-checked here.
	dev, inode, ok := fsx.Identity(info)
	if !ok || inode == 0 {
		return false, nil
	}
	previous, err := store.NodeByInode(ctx, s.db, user.ID, dev, inode, false)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if previous.Path == newPath {
		return false, nil
	}

	// A hard link puts one inode at two live paths. If the old path is still
	// there, this is a second name for the same data, not a move, and taking
	// the ID would corrupt the entry that legitimately holds it.
	if _, err := st.Stat(previous.Path); err == nil {
		return false, nil
	}

	unlock := indexLocks.lock(user.ID)
	defer unlock()
	parent, err := store.NodeByPath(ctx, s.db, user.ID, parentDir(newPath))
	if err != nil {
		return false, nil // parent not indexed yet; treat as a new file
	}
	if err := store.MoveNode(ctx, s.db, user.ID, previous.Path, newPath, parent.ID, path.Base(newPath)); err != nil {
		return false, fmt.Errorf("carry file id across rename: %w", err)
	}
	s.log.Debug("recognised a rename",
		"user", user.Username, "from", previous.Path, "to", newPath, "fileid", previous.ID)
	return true, nil
}

// escapeLikePrefix escapes LIKE wildcards in a path prefix.
func escapeLikePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p)
}

// ScanAll scans every enabled user in turn. reason is recorded in the log so
// that a scan's origin is visible afterwards rather than having to be guessed
// at from timing.
//
// A failure for one user is logged and the rest continue: one bad mount should
// not leave every other tenant unindexed.
func (s *Scanner) ScanAll(ctx context.Context, reason string) error {
	return s.scanAll(ctx, reason, false)
}

// QuickScanAll runs a quick pass over every enabled account.
func (s *Scanner) QuickScanAll(ctx context.Context, reason string) error {
	return s.scanAll(ctx, reason, true)
}

// StartupScan catches up with whatever changed while the server was not
// running, choosing the cheapest pass that can see it.
//
// An account with no index needs a full walk, because there is nothing to
// compare against. An account that already has one does not: what happens to a
// share while a server is down is files appearing, disappearing and being
// renamed, and a quick pass sees all three. Making every restart pay for a full
// walk of a few million files means a restart costs an hour of disk, which is
// enough to discourage restarting at all - and a server nobody dares restart is
// a worse problem than a slightly stale index.
//
// The full walk still runs, on its own interval, and remains the only thing
// that sees a file rewritten in place.
func (s *Scanner) StartupScan(ctx context.Context) error {
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		if u.Disabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		indexed, err := store.CountNodes(ctx, s.db, u.ID)
		if err != nil {
			return fmt.Errorf("count indexed entries for %s: %w", u.Username, err)
		}
		if indexed == 0 {
			s.log.Info("scan starting", "kind", "full", "reason", "first scan for this account",
				"user", u.Username)
			if _, err := s.ScanUser(ctx, u); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				s.log.Error("scan failed", "user", u.Username, "error", err)
			}
			continue
		}

		s.log.Info("scan starting", "kind", "quick", "reason", "server started",
			"user", u.Username, "indexed", indexed)
		if _, err := s.QuickScanUser(ctx, u); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			s.log.Error("quick scan failed", "user", u.Username, "error", err)
		}
	}
	return nil
}

func (s *Scanner) scanAll(ctx context.Context, reason string, quick bool) error {
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	kind := "full"
	if quick {
		kind = "quick"
	}
	s.log.Info("scan starting", "kind", kind, "reason", reason, "accounts", len(users))
	for _, u := range users {
		if u.Disabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		scan := s.ScanUser
		if quick {
			scan = s.QuickScanUser
		}
		if _, err := scan(ctx, u); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			s.log.Error("scan failed", "user", u.Username, "error", err)
		}
	}
	return nil
}

// contentType guesses a MIME type from a filename. Clients use it for icons and
// previews, so a wrong guess is cosmetic.
func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := extraMediaTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// extraMediaTypes fills the gaps in the standard table.
//
// Go's built-in list has no entry for HEIC, and a container has no system
// mime.types file to fall back on, so every photograph an iPhone has taken
// since 2017 was being recorded as an anonymous blob. That is not cosmetic: the
// media view in the mobile apps finds pictures by searching for a content type
// beginning "image/", so without these it finds nothing at all.
//
// Consulted before the standard table so the answer does not depend on which
// mime.types file happens to be present on the host.
var extraMediaTypes = map[string]string{
	".heic": "image/heic",
	".heif": "image/heif",
	".hif":  "image/heif",
	".avif": "image/avif",
	".webp": "image/webp",
	".jxl":  "image/jxl",
	".dng":  "image/x-adobe-dng",
	".cr2":  "image/x-canon-cr2",
	".cr3":  "image/x-canon-cr3",
	".nef":  "image/x-nikon-nef",
	".arw":  "image/x-sony-arw",
	".orf":  "image/x-olympus-orf",
	".raf":  "image/x-fuji-raf",
	".rw2":  "image/x-panasonic-rw2",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".avi":  "video/x-msvideo",
	".3gp":  "video/3gpp",
	".mts":  "video/mp2t",
	".m2ts": "video/mp2t",
}
