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

// UpsertNode inserts or updates an entry, returning its file ID.
//
// The conflict target is (user_id, path), and the update deliberately leaves
// id untouched: an entry that is rewritten in place keeps the file ID clients
// already know it by.
func UpsertNode(ctx context.Context, q Querier, n Node) (int64, error) {
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
		n.ETag, n.ContentType, n.Dev, n.Inode, time.Now().Unix()).Scan(&id)
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
// their parent_id; FinalizeDirNode writes the ETag afterwards. Keeping the old
// ETag in the meantime means an interrupted scan leaves a stale value rather
// than a meaningless one.
func EnsureDirNode(ctx context.Context, q Querier, userID, parentID int64, path, name string) (int64, error) {
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO nodes (user_id, parent_id, path, name, is_dir, etag, scanned_at)
		VALUES (?, ?, ?, ?, 1, '', ?)
		ON CONFLICT(user_id, path) DO UPDATE SET
			parent_id = excluded.parent_id,
			name      = excluded.name,
			is_dir    = 1
		RETURNING id`,
		userID, parent, path, name, time.Now().Unix()).Scan(&id)
	return id, err
}

// FinalizeDirNode writes a directory's derived ETag and recursive size once its
// children have been scanned.
func FinalizeDirNode(ctx context.Context, q Querier, id int64, etag string, size int64, mtime time.Time) error {
	_, err := q.ExecContext(ctx, `
		UPDATE nodes SET etag = ?, size = ?, mtime = ?, scanned_at = ? WHERE id = ?`,
		etag, size, mtime.Unix(), time.Now().Unix(), id)
	return err
}
