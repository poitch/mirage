package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Node is one indexed file or directory.
//
// ID doubles as the file's oc:fileid on the wire. It is assigned once and never
// reused, so a rename updates Path and leaves ID alone; that is what lets a
// client move a file instead of re-downloading it.
type Node struct {
	ID       int64
	UserID   int64
	ParentID int64 // zero for the user's root
	// Path is slash-separated and relative to the user's home, with "." for
	// the root itself.
	Path        string
	Name        string
	IsDir       bool
	Size        int64 // for a directory, the total size of everything beneath it
	MTime       time.Time
	ETag        string
	ContentType string
	Dev         uint64
	Inode       uint64
}

// Querier is satisfied by both *sql.DB and *sql.Tx, so index operations can run
// inside a caller's transaction or standalone.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const nodeColumns = `id, user_id, COALESCE(parent_id, 0), path, name, is_dir, size, mtime, etag, content_type, dev, inode`

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var n Node
	var mtime int64
	err := row.Scan(&n.ID, &n.UserID, &n.ParentID, &n.Path, &n.Name,
		&n.IsDir, &n.Size, &mtime, &n.ETag, &n.ContentType, &n.Dev, &n.Inode)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}
	n.MTime = time.Unix(mtime, 0)
	return n, nil
}

// NodeByPath looks up one indexed entry.
func NodeByPath(ctx context.Context, q Querier, userID int64, path string) (Node, error) {
	return scanNode(q.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE user_id = ? AND path = ?`, userID, path))
}

// NodeByID looks up one indexed entry by its file ID, scoped to a user so a
// guessed ID cannot reach another tenant's file.
func NodeByID(ctx context.Context, q Querier, userID, id int64) (Node, error) {
	return scanNode(q.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE user_id = ? AND id = ?`, userID, id))
}

// ChildNodes lists a directory's immediate contents, ordered by name.
func ChildNodes(ctx context.Context, q Querier, parentID int64) ([]Node, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE parent_id = ? ORDER BY name`, parentID)
	if err != nil {
		return nil, err
	}
	return collectNodes(rows)
}

// SubtreeNodes lists everything beneath a directory, for a Depth: infinity
// request. The root of the subtree is not included.
func SubtreeNodes(ctx context.Context, q Querier, userID int64, path string) ([]Node, error) {
	// LIKE with an escaped prefix, so a directory named "a%b" cannot match
	// unrelated siblings.
	prefix := ""
	if path != "." {
		prefix = path + "/"
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE user_id = ? AND path <> '.' AND path LIKE ? ESCAPE '\'
		 ORDER BY path`,
		userID, escapeLike(prefix)+"%")
	if err != nil {
		return nil, err
	}
	return collectNodes(rows)
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func collectNodes(rows *sql.Rows) ([]Node, error) {
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Stamp returns a marker for the current instant, in the units scanned_at uses.
//
// Nanoseconds rather than seconds because a scan can begin and finish inside
// one second, and a stamp that did not advance would make the sweep below
// delete entries the scan had just written.
func Stamp() int64 { return time.Now().UnixNano() }

// UpsertNode inserts or updates an entry, returning its file ID.
//
// The conflict target is (user_id, path), and the update deliberately leaves
// id untouched: an entry that is rewritten in place keeps the file ID clients
// already know it by.
func UpsertNode(ctx context.Context, q Querier, n Node, stamp int64) (int64, error) {
	var parent any
	if n.ParentID != 0 {
		parent = n.ParentID
	}
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO nodes (user_id, parent_id, path, name, is_dir, size, mtime,
		                   etag, content_type, dev, inode, scanned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, path) DO UPDATE SET
			parent_id    = excluded.parent_id,
			name         = excluded.name,
			is_dir       = excluded.is_dir,
			size         = excluded.size,
			mtime        = excluded.mtime,
			etag         = excluded.etag,
			content_type = excluded.content_type,
			dev          = excluded.dev,
			inode        = excluded.inode,
			scanned_at   = excluded.scanned_at
		RETURNING id`,
		n.UserID, parent, n.Path, n.Name, n.IsDir, n.Size, n.MTime.Unix(),
		n.ETag, n.ContentType, n.Dev, n.Inode, stamp).Scan(&id)
	return id, err
}

// DeleteMissingChildren removes indexed entries under parentID whose names are
// no longer present on disk. Subtrees go with them via the parent_id cascade.
func DeleteMissingChildren(ctx context.Context, q Querier, parentID int64, keep []string) error {
	if len(keep) == 0 {
		_, err := q.ExecContext(ctx, `DELETE FROM nodes WHERE parent_id = ?`, parentID)
		return err
	}
	args := make([]any, 0, len(keep)+1)
	args = append(args, parentID)
	for _, name := range keep {
		args = append(args, name)
	}
	query := `DELETE FROM nodes WHERE parent_id = ? AND name NOT IN (?` +
		strings.Repeat(", ?", len(keep)-1) + `)`
	_, err := q.ExecContext(ctx, query, args...)
	return err
}

// DeleteNode removes an entry and everything beneath it.
func DeleteNode(ctx context.Context, q Querier, userID int64, path string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM nodes WHERE user_id = ? AND path = ?`, userID, path)
	return err
}

// UserUsage returns the total bytes a user occupies, taken from the recursive
// size held on their root node. It is zero before the first scan completes.
func UserUsage(ctx context.Context, q Querier, userID int64) (int64, error) {
	var size int64
	err := q.QueryRowContext(ctx,
		`SELECT size FROM nodes WHERE user_id = ? AND path = '.'`, userID).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return size, err
}

// CountNodes returns how many entries are indexed for a user.
func CountNodes(ctx context.Context, q Querier, userID int64) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// EnsureDirNode inserts a directory entry if it is absent and returns its file
// ID, leaving any existing ETag alone.
//
// A directory's ETag is derived from its children, so it cannot be known until
// they have been scanned. This establishes the identity the children need for
// their parent_id; FinalizeDirNode writes the real ETag afterwards.
//
// The provisional values matter more than they look. Between this call and
// finalizing, the row is visible to clients - a PROPFIND arriving mid-scan, or
// a directory whose contents could not be read at all, sees whatever is stored
// here. Left to column defaults that is a modification time of zero, which
// clients render as 1970, and an empty ETag, which is not a valid ETag at all.
// So the directory's own timestamp is written up front, and the ETag is seeded
// from it: wrong until the children are counted, but well-formed and roughly
// right rather than nonsense.
//
// On conflict a real value is left alone, so a rescan does not clobber a
// correct ETag with a provisional one - but a provisional value that is still
// sitting there is repaired. A scan interrupted before it finished a directory
// leaves exactly that, and without the repair those rows would keep reporting
// 1970 and an empty ETag until some future scan happened to complete.
func EnsureDirNode(ctx context.Context, q Querier, userID, parentID int64, path, name string,
	mtime time.Time, provisionalETag string, stamp int64) (int64, error) {
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO nodes (user_id, parent_id, path, name, is_dir, mtime, etag, scanned_at)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT(user_id, path) DO UPDATE SET
			parent_id  = excluded.parent_id,
			name       = excluded.name,
			is_dir     = 1,
			etag       = CASE WHEN nodes.etag = ''  THEN excluded.etag  ELSE nodes.etag  END,
			mtime      = CASE WHEN nodes.mtime <= 0 THEN excluded.mtime ELSE nodes.mtime END,
			scanned_at = excluded.scanned_at
		RETURNING id`,
		userID, parent, path, name, mtime.Unix(), provisionalETag, stamp).Scan(&id)
	return id, err
}

// FinalizeDirNode writes a directory's derived ETag and recursive size once its
// children have been scanned.
func FinalizeDirNode(ctx context.Context, q Querier, id int64, etag string, size int64, mtime time.Time, stamp int64) error {
	_, err := q.ExecContext(ctx, `
		UPDATE nodes SET etag = ?, size = ?, mtime = ?, scanned_at = ? WHERE id = ?`,
		etag, size, mtime.Unix(), stamp, id)
	return err
}

// NodeByInode finds an indexed entry by its filesystem identity.
//
// This is how an out-of-band rename is told apart from a delete plus a create:
// the same (dev, inode) appearing at a new path means the file moved, and its
// file ID should follow it.
func NodeByInode(ctx context.Context, q Querier, userID int64, dev, inode uint64) (Node, error) {
	if inode == 0 {
		// Filesystems that report no inode cannot support this.
		return Node{}, ErrNotFound
	}
	return scanNode(q.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE user_id = ? AND dev = ? AND inode = ? AND is_dir = 0
		 LIMIT 1`, userID, dev, inode))
}

// MarkSubtreeScanned stamps a directory and everything beneath it as seen.
//
// It exists for a directory that could not be read: the scan has no idea what
// is inside, so the whole subtree must be preserved rather than swept.
func MarkSubtreeScanned(ctx context.Context, q Querier, userID int64, path string, stamp int64) error {
	_, err := q.ExecContext(ctx, `
		UPDATE nodes SET scanned_at = ?
		WHERE user_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
		stamp, userID, path, escapeLike(path+"/")+"%")
	return err
}

// SweepUnscanned deletes entries a completed scan did not touch, and reports
// how many went.
//
// Pruning happens once at the end rather than per directory so that a file
// which moved is still in the index when its new location is reached. Without
// that, whether a rename was noticed would depend on the order the tree
// happened to be walked in.
func SweepUnscanned(ctx context.Context, q Querier, userID int64, stamp int64) (int64, error) {
	res, err := q.ExecContext(ctx,
		`DELETE FROM nodes WHERE user_id = ? AND scanned_at < ?`, userID, stamp)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MoveNode relocates an entry and rewrites the paths of everything beneath it.
//
// File IDs are deliberately left alone, for the node and every descendant.
// That is the whole point of a stable ID: a client that sees a familiar ID at a
// new path performs a local rename, where a new ID would make it delete and
// re-download the entire subtree.
func MoveNode(ctx context.Context, q Querier, userID int64, oldPath, newPath string, newParentID int64, newName string) error {
	var parent any
	if newParentID != 0 {
		parent = newParentID
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE nodes SET path = ?, name = ?, parent_id = ?
		WHERE user_id = ? AND path = ?`,
		newPath, newName, parent, userID, oldPath); err != nil {
		return err
	}

	// Descendants keep their parent_id chain; only the textual path shifts.
	// substr is 1-indexed, so len(oldPath)+1 starts at the separator that
	// follows the old prefix.
	_, err := q.ExecContext(ctx, `
		UPDATE nodes SET path = ? || substr(path, ?)
		WHERE user_id = ? AND path LIKE ? ESCAPE '\'`,
		newPath, len(oldPath)+1, userID, escapeLike(oldPath+"/")+"%")
	return err
}
