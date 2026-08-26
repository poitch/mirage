package index

import (
	"context"
	"errors"
	"fmt"
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
	db      *store.DB
	storage *fsx.Manager
	log     *slog.Logger
}

// NewScanner builds a Scanner.
func NewScanner(db *store.DB, storage *fsx.Manager, log *slog.Logger) *Scanner {
	return &Scanner{db: db, storage: storage, log: log}
}

// Stats summarises one scan.
type Stats struct {
	Files    int64
	Dirs     int64
	Bytes    int64
	Skipped  int64 // directories that could not be read and were left untouched
	Duration time.Duration
}

// ScanUser walks a user's home directory and brings their index up to date.
func (s *Scanner) ScanUser(ctx context.Context, user store.User) (Stats, error) {
	start := time.Now()
	var stats Stats

	st, err := s.storage.For(user.ID, user.Home, user.UID, user.GID)
	if err != nil {
		return stats, fmt.Errorf("open storage for %s: %w", user.Username, err)
	}

	if _, _, err := s.scanDir(ctx, st, user, fsx.RootPath, 0, &stats); err != nil {
		return stats, err
	}

	stats.Duration = time.Since(start)
	s.log.Info("scan complete",
		"user", user.Username, "files", stats.Files, "dirs", stats.Dirs,
		"bytes", stats.Bytes, "skipped", stats.Skipped, "duration", stats.Duration)
	return stats, nil
}

// scanDir indexes one directory and everything beneath it, returning the
// directory's derived ETag and its recursive size.
func (s *Scanner) scanDir(ctx context.Context, st *fsx.Storage, user store.User,
	dirPath string, parentID int64, stats *Stats) (etag string, size int64, err error) {

	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	info, err := st.Stat(dirPath)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", dirPath, err)
	}

	name := path.Base(dirPath)
	if fsx.IsRoot(dirPath) {
		name = ""
	}

	dirID, err := store.EnsureDirNode(ctx, s.db, user.ID, parentID, dirPath, name)
	if err != nil {
		return "", 0, fmt.Errorf("index directory %s: %w", dirPath, err)
	}

	entries, err := st.ReadDir(dirPath)
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
		existing, lookupErr := store.NodeByPath(ctx, s.db, user.ID, dirPath)
		if lookupErr != nil {
			return "", 0, nil
		}
		return existing.ETag, existing.Size, nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	digests := make([]ChildDigest, 0, len(entries))
	keep := make([]string, 0, len(entries))
	var total int64

	for _, entry := range entries {
		childName := entry.Name()
		childPath := fsx.Join(dirPath, childName)
		keep = append(keep, childName)

		if entry.IsDir() {
			childETag, childSize, err := s.scanDir(ctx, st, user, childPath, dirID, stats)
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
			keep = keep[:len(keep)-1]
			continue
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
		}); err != nil {
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

	if err := store.DeleteMissingChildren(ctx, s.db, dirID, keep); err != nil {
		return "", 0, fmt.Errorf("prune %s: %w", dirPath, err)
	}

	etag = DirETag(digests)
	if err := store.FinalizeDirNode(ctx, s.db, dirID, etag, total, info.ModTime()); err != nil {
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

	var stats Stats
	if info.IsDir() {
		if _, _, err := s.scanDir(ctx, st, user, target, parentID, &stats); err != nil {
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
		})
		unlock()
		if err != nil {
			return fmt.Errorf("index %s: %w", target, err)
		}
	}

	unlock := indexLocks.lock(user.ID)
	defer unlock()
	return propagate(ctx, s.db, user.ID, parentPath)
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
