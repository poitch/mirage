package index

import (
	"context"
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
}

// SetNotifier attaches a change notifier. Passing nil disables notification.
func (s *Scanner) SetNotifier(n Notifier) { s.notifier = n }

// NewScanner builds a Scanner.
func NewScanner(db *store.DB, storage *fsx.Manager, log *slog.Logger) *Scanner {
	return &Scanner{db: db, storage: storage, log: log}
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
	Duration     time.Duration
}

// ScanUser walks a user's home directory and brings their index up to date.
func (s *Scanner) ScanUser(ctx context.Context, user store.User) (Stats, error) {
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
	stamp := store.Stamp()

	// The root ETag summarises the whole tree, so comparing it before and after
	// is an exact test of whether this scan found anything. That matters
	// because a scan runs on a timer: notifying unconditionally would wake
	// every client every interval for nothing.
	before := s.rootETag(ctx, user.ID)

	progress := s.newProgress(user.Username)
	if _, _, err := s.scanDir(ctx, st, user, fsx.RootPath, 0, stamp, &stats, progress); err != nil {
		return stats, err
	}
	progress.flush(ctx, &stats, "", true)

	removed, err := store.SweepUnscanned(ctx, s.db, user.ID, stamp)
	if err != nil {
		return stats, fmt.Errorf("prune index for %s: %w", user.Username, err)
	}
	stats.Removed = removed

	if after := s.rootETag(ctx, user.ID); after != before && s.notifier != nil {
		s.notifier.FileChanged(user.ID, nil)
	}

	stats.Duration = time.Since(start)
	s.log.Info("scan complete",
		"user", user.Username, "files", stats.Files, "dirs", stats.Dirs,
		"bytes", stats.Bytes, "moved", stats.Moved, "removed", stats.Removed,
		"skipped", stats.Skipped, "skipped_links", stats.SkippedLinks,
		"duration", stats.Duration)
	return stats, nil
}

// scanDir indexes one directory and everything beneath it, returning the
// directory's derived ETag and its recursive size.
func (s *Scanner) scanDir(ctx context.Context, st *fsx.Storage, user store.User,
	dirPath string, parentID int64, stamp int64, stats *Stats, progress *progressReporter) (etag string, size int64, err error) {

	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if progress != nil {
		progress.update(ctx, stats, dirPath)
	}

	info, err := st.Stat(dirPath)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", dirPath, err)
	}

	name := path.Base(dirPath)
	if fsx.IsRoot(dirPath) {
		name = ""
	}

	// Seeded from the directory's own metadata so that a client reading this
	// row before the scan reaches the end of it sees something sensible.
	dirID, err := store.EnsureDirNode(ctx, s.db, user.ID, parentID, dirPath, name,
		info.ModTime(), FileETag(0, info.ModTime()), stamp)
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

	digests := make([]ChildDigest, 0, len(entries))
	var total int64

	for _, entry := range entries {
		childName := entry.Name()
		childPath := fsx.Join(dirPath, childName)

		if entry.IsDir() {
			childETag, childSize, err := s.scanDir(ctx, st, user, childPath, dirID, stamp, stats, progress)
			if err != nil {
				return "", 0, err
			}
			digests = append(digests, ChildDigest{Name: childName, ETag: childETag})
			total += childSize
			stats.Dirs++
			continue
		}

		childInfo, err := entry.Info()
		if err != nil {
			// The file vanished between listing and stat. Drop it from this
			// pass rather than indexing something we could not measure.
			s.log.Debug("entry disappeared during scan", "path", childPath, "error", err)
			continue
		}

		if moved, err := s.detectRename(ctx, st, user, childPath, childInfo); err != nil {
			return "", 0, err
		} else if moved {
			stats.Moved++
		}

		childETag := FileETag(childInfo.Size(), childInfo.ModTime())
		if _, err := store.UpsertNode(ctx, s.db, store.Node{
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
		}, stamp); err != nil {
			return "", 0, fmt.Errorf("index file %s: %w", childPath, err)
		}

		digests = append(digests, ChildDigest{Name: childName, ETag: childETag})
		total += childInfo.Size()
		stats.Files++
		stats.Bytes += childInfo.Size()
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
		if _, _, err := s.scanDir(ctx, st, user, target, parentID, stamp, &stats, nil); err != nil {
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
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM nodes
		WHERE user_id = ? AND scanned_at < ? AND path LIKE ? ESCAPE '\'`,
		user.ID, stamp, escapeLikePrefix(target)+"/%")
	return err
}

// detectRename checks whether a file that is new at this path is really one
// that moved, and if so carries its index entry - and its file ID - across.
//
// Clients use the file ID to tell a rename from a delete plus a create. Without
// this, renaming a large folder over SMB would make every client delete and
// re-download all of it.
func (s *Scanner) detectRename(ctx context.Context, st *fsx.Storage, user store.User,
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
	previous, err := store.NodeByInode(ctx, s.db, user.ID, dev, inode)
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

// ScanAll scans every enabled user in turn.
//
// A failure for one user is logged and the rest continue: one bad mount should
// not leave every other tenant unindexed.
func (s *Scanner) ScanAll(ctx context.Context) error {
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
		if _, err := s.ScanUser(ctx, u); err != nil {
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
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(name))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
