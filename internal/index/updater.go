package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// indexLocks serialises index mutation per user.
//
// It is shared by the Updater and the Scanner because both derive a directory's
// ETag by reading its children and writing back the result. Two of those
// running at once on the same directory could leave a parent describing a state
// that never existed. Keying by user keeps unrelated tenants independent.
var indexLocks keyedMutex

// Notifier is told when a user's files have changed, so that clients can be
// woken rather than left to discover it on their next poll.
//
// Implementations must not block: they are called with the index lock held.
type Notifier interface {
	FileChanged(userID int64, fileIDs []int64)
}

// Updater applies single changes to the index and propagates their effect
// upwards, without rescanning the tree.
//
// A scan is how the index is rebuilt; this is how it keeps up with writes the
// server itself performs.
type Updater struct {
	db       *store.DB
	notifier Notifier
}

// NewUpdater builds an Updater.
func NewUpdater(db *store.DB) *Updater {
	return &Updater{db: db}
}

// SetNotifier attaches a change notifier. Passing nil disables notification.
func (u *Updater) SetNotifier(n Notifier) { u.notifier = n }

// notify reports a change, if anyone is listening.
func (u *Updater) notify(userID int64, fileIDs ...int64) {
	if u.notifier != nil {
		u.notifier.FileChanged(userID, fileIDs)
	}
}

// FileWritten records a created or replaced file and returns its indexed form.
func (u *Updater) FileWritten(ctx context.Context, user store.User, filePath string,
	size int64, mtime time.Time) (store.Node, error) {

	unlock := indexLocks.lock(user.ID)
	defer unlock()

	var node store.Node
	err := u.db.Tx(ctx, func(tx *sql.Tx) error {
		parent, err := u.ensureParent(ctx, tx, user, filePath)
		if err != nil {
			return err
		}
		name := path.Base(filePath)
		id, err := store.UpsertNode(ctx, tx, store.Node{
			UserID:      user.ID,
			ParentID:    parent.ID,
			Path:        filePath,
			Name:        name,
			IsDir:       false,
			Size:        size,
			MTime:       mtime,
			ETag:        FileETag(size, mtime),
			ContentType: contentType(name),
		}, store.Stamp())
		if err != nil {
			return err
		}
		if err := propagate(ctx, tx, user.ID, path.Dir(filePath)); err != nil {
			return err
		}
		node, err = store.NodeByID(ctx, tx, user.ID, id)
		return err
	})
	if err == nil {
		u.notify(user.ID, node.ID)
	}
	return node, err
}

// DirCreated records a new directory.
func (u *Updater) DirCreated(ctx context.Context, user store.User, dirPath string) (store.Node, error) {
	unlock := indexLocks.lock(user.ID)
	defer unlock()

	var node store.Node
	err := u.db.Tx(ctx, func(tx *sql.Tx) error {
		parent, err := u.ensureParent(ctx, tx, user, dirPath)
		if err != nil {
			return err
		}
		id, err := store.EnsureDirNode(ctx, tx, user.ID, parent.ID, dirPath, path.Base(dirPath), store.Stamp())
		if err != nil {
			return err
		}
		// A new directory is empty, so its ETag is the empty-children digest.
		if err := store.FinalizeDirNode(ctx, tx, id, DirETag(nil), 0, time.Now(), store.Stamp()); err != nil {
			return err
		}
		if err := propagate(ctx, tx, user.ID, path.Dir(dirPath)); err != nil {
			return err
		}
		node, err = store.NodeByID(ctx, tx, user.ID, id)
		return err
	})
	if err == nil {
		u.notify(user.ID, node.ID)
	}
	return node, err
}

// Removed drops an entry and everything beneath it.
func (u *Updater) Removed(ctx context.Context, user store.User, targetPath string) error {
	unlock := indexLocks.lock(user.ID)
	defer unlock()

	err := u.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := store.DeleteNode(ctx, tx, user.ID, targetPath); err != nil {
			return err
		}
		return propagate(ctx, tx, user.ID, path.Dir(targetPath))
	})
	if err == nil {
		// No file id: the entry is gone, so there is nothing to identify. The
		// protocol falls back to a bare "something changed", which is all a
		// client needs to go and look.
		u.notify(user.ID)
	}
	return err
}

// Moved relocates an entry, keeping its file ID.
func (u *Updater) Moved(ctx context.Context, user store.User, oldPath, newPath string) error {
	unlock := indexLocks.lock(user.ID)
	defer unlock()

	err := u.db.Tx(ctx, func(tx *sql.Tx) error {
		// Anything already at the destination is being replaced.
		if err := store.DeleteNode(ctx, tx, user.ID, newPath); err != nil {
			return err
		}
		parent, err := u.ensureParent(ctx, tx, user, newPath)
		if err != nil {
			return err
		}
		if err := store.MoveNode(ctx, tx, user.ID, oldPath, newPath, parent.ID, path.Base(newPath)); err != nil {
			return err
		}
		// Both ends of the move change: the source directory lost an entry and
		// the destination gained one.
		if err := propagate(ctx, tx, user.ID, path.Dir(oldPath)); err != nil {
			return err
		}
		return propagate(ctx, tx, user.ID, path.Dir(newPath))
	})
	if err == nil {
		u.notify(user.ID)
	}
	return err
}

// ensureParent returns the indexed directory that should contain a path.
func (u *Updater) ensureParent(ctx context.Context, q store.Querier, user store.User, childPath string) (store.Node, error) {
	parentPath := path.Dir(childPath)
	if parentPath == "" || parentPath == "/" {
		parentPath = fsx.RootPath
	}
	parent, err := store.NodeByPath(ctx, q, user.ID, parentPath)
	if err == nil {
		return parent, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Node{}, err
	}
	return store.Node{}, fmt.Errorf("parent directory %q is not indexed: %w", parentPath, store.ErrNotFound)
}

// propagate recomputes directory ETags from a directory up to the root.
//
// This is the write-path counterpart to what a full scan does bottom-up. A
// client will not descend into a directory whose ETag it already knows, so
// every ancestor of a change has to be refreshed or the change stays invisible.
func propagate(ctx context.Context, q store.Querier, userID int64, fromDir string) error {
	dir := fromDir
	if dir == "" || dir == "/" {
		dir = fsx.RootPath
	}
	for {
		node, err := store.NodeByPath(ctx, q, userID, dir)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The directory is not indexed, so there is nothing above it
				// to refresh either.
				return nil
			}
			return err
		}

		children, err := store.ChildNodes(ctx, q, node.ID)
		if err != nil {
			return err
		}
		digests := make([]ChildDigest, 0, len(children))
		var total int64
		for _, c := range children {
			digests = append(digests, ChildDigest{Name: c.Name, ETag: c.ETag})
			total += c.Size
		}
		if err := store.FinalizeDirNode(ctx, q, node.ID, DirETag(digests), total, time.Now(), store.Stamp()); err != nil {
			return err
		}

		if fsx.IsRoot(dir) {
			return nil
		}
		dir = parentDir(dir)
	}
}

// parentDir returns the containing directory of a cleaned path.
func parentDir(p string) string {
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return fsx.RootPath
	}
	return p[:idx]
}

// keyedMutex serialises index updates per user.
//
// Propagation reads a directory's children and writes back a derived ETag, so
// two concurrent writes under one user could otherwise interleave and leave a
// parent describing a state that never existed. Locking per user keeps
// unrelated tenants independent.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func (k *keyedMutex) lock(key int64) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[int64]*sync.Mutex)
	}
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()

	m.Lock()
	return m.Unlock
}
