// Package versions keeps earlier copies of files that are overwritten.
//
// Both the sync endpoints and the browser view need this, and they need it to
// behave identically: restoring a version from a phone and restoring it from a
// browser must leave the same history behind. So the rule lives here rather
// than in whichever of them was written first.
package versions

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

// Policy says whether earlier copies are kept, and what that is allowed to
// cost.
type Policy struct {
	Enabled bool
	// Retention discards versions older than this.
	Retention time.Duration
	// MaxPerFile caps how many are kept for one file. A document saved every
	// few minutes all day would otherwise accumulate hundreds of copies.
	MaxPerFile int
	// MaxFileSize is the largest file that gets a version at all.
	//
	// Keeping one means copying the file: a hard link would share an inode with
	// the live file, so anything writing in place - which is what SMB clients
	// and many editors do - would rewrite the history along with it. Without a
	// bound, one large video saved twice costs more than an account's whole
	// document history.
	MaxFileSize int64
}

// Keeper puts earlier copies aside according to a policy.
type Keeper struct {
	db     *store.DB
	policy Policy
	log    *slog.Logger
}

// NewKeeper builds a Keeper. A disabled policy makes Keep do nothing, which
// lets callers use it unconditionally.
func NewKeeper(db *store.DB, policy Policy, log *slog.Logger) *Keeper {
	return &Keeper{db: db, policy: policy, log: log}
}

// Enabled reports whether earlier copies are kept.
func (k *Keeper) Enabled() bool { return k != nil && k.policy.Enabled }

// Keep copies a file's current contents aside before something overwrites them.
//
// Files above the configured size are skipped, and that is reported as success:
// keeping a version of a five gigabyte video would cost more than the account's
// entire document history, and failing the upload over it would be worse still.
func (k *Keeper) Keep(ctx context.Context, st *fsx.Storage, user store.User, node store.Node) error {
	if !k.Enabled() || node.IsDir {
		return nil
	}
	if k.policy.MaxFileSize > 0 && node.Size > k.policy.MaxFileSize {
		k.log.Debug("not keeping a version; the file is over the size limit",
			"user", user.Username, "path", node.Path, "size", node.Size)
		return nil
	}
	// An empty file has nothing worth keeping, and a save that only touches the
	// timestamp would otherwise fill the history with identical copies.
	if node.Size == 0 {
		return nil
	}

	// A version is addressed by whole seconds, because that is the resolution
	// clients use. The file's own modification time is the natural stamp - it
	// says when these contents were current - but two edits can share a second,
	// and collapsing them would silently discard one. So a taken stamp moves to
	// the next free second: slightly inaccurate, where the alternative loses
	// somebody's work.
	stamp := node.MTime.Truncate(time.Second)
	for range 60 {
		_, err := store.VersionAt(ctx, k.db, user.ID, node.ID, stamp)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return err
		}
		stamp = stamp.Add(time.Second)
	}

	size, err := st.SaveVersion(node.Path, node.ID, stamp.Unix())
	if err != nil {
		return err
	}
	if _, err := store.AddVersion(ctx, k.db, user.ID, store.Version{
		NodeID: node.ID, Path: node.Path, Timestamp: stamp, Size: size,
	}); err != nil {
		// The copy exists but nothing knows about it, which would leave it
		// occupying disk forever. Removing it is the only tidy outcome.
		if rmErr := st.RemoveVersion(node.ID, stamp.Unix()); rmErr != nil {
			k.log.Error("a version was written but could not be recorded or removed",
				"user", user.Username, "path", node.Path, "error", rmErr)
		}
		return err
	}

	k.trim(ctx, st, user, node.ID)
	return nil
}

// trim discards the oldest copies of a file beyond the limit.
//
// Best effort: failing to trim is not a reason to fail the save that triggered
// it, and the periodic sweep catches whatever is left.
func (k *Keeper) trim(ctx context.Context, st *fsx.Storage, user store.User, nodeID int64) {
	if k.policy.MaxPerFile <= 0 {
		return
	}
	surplus, err := store.SurplusVersions(ctx, k.db, user.ID, nodeID, k.policy.MaxPerFile)
	if err != nil {
		k.log.Warn("could not read surplus versions", "user", user.Username, "error", err)
		return
	}
	for _, v := range surplus {
		if err := st.RemoveVersion(v.NodeID, v.Timestamp.Unix()); err != nil {
			k.log.Warn("could not remove a surplus version",
				"user", user.Username, "path", v.Path, "error", err)
		}
		if err := store.RemoveVersion(ctx, k.db, user.ID, v.ID); err != nil {
			k.log.Warn("could not forget a surplus version",
				"user", user.Username, "path", v.Path, "error", err)
		}
	}
}
