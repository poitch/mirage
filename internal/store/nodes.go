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
	// ScannedAt is the generation marker of the scan that last touched this
	// entry, and Complete reports whether that scan finished reading it. For a
	// directory the two together answer "was this subtree finished during the
	// current pass", which is what makes an interrupted scan resumable.
	ScannedAt int64
	Complete  bool
}

// Querier is satisfied by both *sql.DB and *sql.Tx, so index operations can run
// inside a caller's transaction or standalone.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const nodeColumns = `id, user_id, COALESCE(parent_id, 0), path, name, is_dir, size, mtime, etag, content_type, dev, inode, scanned_at, complete`

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var n Node
	var mtime int64
	err := row.Scan(&n.ID, &n.UserID, &n.ParentID, &n.Path, &n.Name,
		&n.IsDir, &n.Size, &mtime, &n.ETag, &n.ContentType, &n.Dev, &n.Inode,
		&n.ScannedAt, &n.Complete)
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
	if path == "." {
		rows, err := q.QueryContext(ctx,
			`SELECT `+nodeColumns+` FROM nodes
			 WHERE user_id = ? AND path <> '.' ORDER BY path`, userID)
		if err != nil {
			return nil, err
		}
		return collectNodes(rows)
	}

	lo, hi, ok := PrefixRange(path + "/")
	if !ok {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE user_id = ? AND path >= ? AND path < ? ORDER BY path`,
		userID, lo, hi)
	if err != nil {
		return nil, err
	}
	return collectNodes(rows)
}

// prefixRange turns a path prefix into the half-open range [lo, hi) that
// contains exactly the paths starting with it.
//
// This exists instead of LIKE because SQLite will not use an index for a LIKE
// pattern unless the column's collation is NOCASE - its LIKE is
// case-insensitive for ASCII by default, which disables the optimisation. Every
// subtree query was therefore scanning the whole table, which is invisible
// until the table has a million rows in it. A range comparison uses the
// (user_id, path) index directly.
//
// hi is the prefix with its last byte incremented, so it sorts immediately
// after every string beginning with the prefix.
func PrefixRange(prefix string) (lo, hi string, ok bool) {
	if prefix == "" {
		return "", "", false
	}
	upper := []byte(prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] < 0xFF {
			upper[i]++
			return prefix, string(upper[:i+1]), true
		}
	}
	// Every byte was 0xFF, so nothing sorts after it; the caller falls back to
	// an open-ended range.
	return prefix, "", false
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
		                   etag, content_type, dev, inode, scanned_at, complete)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
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
			scanned_at   = excluded.scanned_at,
			complete     = 1
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
	mtime time.Time, provisionalETag string, dev, inode uint64, stamp int64) (int64, error) {
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO nodes (user_id, parent_id, path, name, is_dir, mtime, etag, dev, inode, scanned_at)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, path) DO UPDATE SET
			parent_id  = excluded.parent_id,
			name       = excluded.name,
			is_dir     = 1,
			etag       = CASE WHEN nodes.etag = ''  THEN excluded.etag  ELSE nodes.etag  END,
			mtime      = CASE WHEN nodes.mtime <= 0 THEN excluded.mtime ELSE nodes.mtime END,
			-- Reading a directory again means its contents are being re-read,
			-- so it is no longer known-complete until finalised once more.
			complete   = 0,
			-- Recorded so that a directory carried across a rename can be
			-- recognised by its filesystem identity, exactly as a file is.
			dev        = excluded.dev,
			inode      = excluded.inode,
			scanned_at = excluded.scanned_at
		RETURNING id`,
		userID, parent, path, name, mtime.Unix(), provisionalETag, dev, inode, stamp).Scan(&id)
	return id, err
}

// FinalizeDirNode writes a directory's derived ETag and recursive size once its
// children have been scanned.
// FinalizeDirNode writes a directory's derived ETag and recursive size once its
// children have been scanned, and marks the subtree as fully read.
func FinalizeDirNode(ctx context.Context, q Querier, id int64, etag string, size int64, mtime time.Time, stamp int64) error {
	_, err := q.ExecContext(ctx, `
		UPDATE nodes SET etag = ?, size = ?, mtime = ?, mtime_nsec = ?, scanned_at = ?, complete = 1
		WHERE id = ?`,
		etag, size, mtime.Unix(), mtime.Nanosecond(), stamp, id)
	return err
}

// NodeByInode finds an indexed entry by its filesystem identity.
//
// This is how an out-of-band rename is told apart from a delete plus a create:
// the same (dev, inode) appearing at a new path means the entry moved, and its
// file ID should follow it.
//
// isDir selects which kind to look for, because the two are asked about at
// different moments and matching a directory against a file would be wrong.
func NodeByInode(ctx context.Context, q Querier, userID int64, dev, inode uint64, isDir bool) (Node, error) {
	if inode == 0 {
		// Filesystems that report no inode cannot support this.
		return Node{}, ErrNotFound
	}
	return scanNode(q.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes
		 WHERE user_id = ? AND dev = ? AND inode = ? AND is_dir = ?
		 LIMIT 1`, userID, dev, inode, isDir))
}

// MarkSubtreeScanned stamps a directory and everything beneath it as seen.
//
// It exists for a directory that could not be read: the scan has no idea what
// is inside, so the whole subtree must be preserved rather than swept.
func MarkSubtreeScanned(ctx context.Context, q Querier, userID int64, path string, stamp int64) error {
	if path == "." {
		_, err := q.ExecContext(ctx,
			`UPDATE nodes SET scanned_at = ? WHERE user_id = ?`, stamp, userID)
		return err
	}
	lo, hi, ok := PrefixRange(path + "/")
	if !ok {
		_, err := q.ExecContext(ctx,
			`UPDATE nodes SET scanned_at = ? WHERE user_id = ? AND path = ?`, stamp, userID, path)
		return err
	}
	_, err := q.ExecContext(ctx, `
		UPDATE nodes SET scanned_at = ?
		WHERE user_id = ? AND (path = ? OR (path >= ? AND path < ?))`,
		stamp, userID, path, lo, hi)
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
	lo, hi, ok := PrefixRange(oldPath + "/")
	if !ok {
		return nil
	}
	_, err := q.ExecContext(ctx, `
		UPDATE nodes SET path = ? || substr(path, ?)
		WHERE user_id = ? AND path >= ? AND path < ?`,
		newPath, len(oldPath)+1, userID, lo, hi)
	return err
}

// touchChunk bounds how many ids go into one statement, since SQLite limits
// how many parameters a query may bind.
const touchChunk = 400

// TouchNodes moves entries forward to the current scan generation without
// rewriting them.
//
// A rescan of an unchanged tree would otherwise rewrite every row in the
// database to store values identical to the ones already there. All those rows
// need is to not look stale to the end-of-scan sweep.
func TouchNodes(ctx context.Context, q Querier, ids []int64, stamp int64) error {
	for start := 0; start < len(ids); start += touchChunk {
		end := min(start+touchChunk, len(ids))
		batch := ids[start:end]

		args := make([]any, 0, len(batch)+1)
		args = append(args, stamp)
		for _, id := range batch {
			args = append(args, id)
		}
		query := `UPDATE nodes SET scanned_at = ? WHERE id IN (?` +
			strings.Repeat(", ?", len(batch)-1) + `)`
		if _, err := q.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// DirState is the little a quick pass needs to know about an indexed directory.
type DirState struct {
	ID    int64
	MTime time.Time
	ETag  string
	Size  int64
	// NsecKnown is false for rows written before sub-second precision was
	// recorded, where MTime is only accurate to the second.
	NsecKnown bool
}

// Changed reports whether an on-disk timestamp differs from the recorded one,
// at the best precision available for that row.
func (d DirState) Changed(onDisk time.Time) bool {
	if d.MTime.Unix() != onDisk.Unix() {
		return true
	}
	if !d.NsecKnown {
		// Only seconds were recorded, so a change inside this second cannot be
		// distinguished. Reported as unchanged; the full rescan is the backstop.
		return false
	}
	return d.MTime.Nanosecond() != onDisk.Nanosecond()
}

// RecentDirs returns a user's most recently modified directories, newest first.
//
// Filesystem watches are a scarce, fixed resource: the kernel's default allows
// a few thousand, while a large share has hundreds of thousands of directories.
// Spending them on whichever directories a tree walk happened to reach first
// wastes them on archives that have not changed in years. The index already
// knows when each directory last changed, so the ones worth watching can simply
// be asked for.
func RecentDirs(ctx context.Context, q Querier, userID int64, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT path FROM nodes
		WHERE user_id = ? AND is_dir = 1
		ORDER BY mtime DESC
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

// IndexedDirs returns every indexed directory for a user, keyed by path.
//
// Fetched in one query so that a quick pass can walk the whole tree comparing
// timestamps without touching the database again. Asking per directory would
// put the database back in the inner loop, which is the cost the quick pass
// exists to avoid.
func IndexedDirs(ctx context.Context, q Querier, userID int64) (map[string]DirState, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT path, id, mtime, mtime_nsec, etag, size FROM nodes WHERE user_id = ? AND is_dir = 1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]DirState)
	for rows.Next() {
		var path string
		var d DirState
		var mtime, nsec int64
		if err := rows.Scan(&path, &d.ID, &mtime, &nsec, &d.ETag, &d.Size); err != nil {
			return nil, err
		}
		d.MTime = time.Unix(mtime, nsec)
		d.NsecKnown = nsec != 0
		out[path] = d
	}
	return out, rows.Err()
}

// CountDirs returns how many directories are indexed for a user.
func CountDirs(ctx context.Context, q Querier, userID int64) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE user_id = ? AND is_dir = 1`, userID).Scan(&n)
	return n, err
}

// SearchNodes finds entries whose name matches a LIKE pattern, within a scope.
//
// The match is on the name rather than the full path, which is what somebody
// typing into a search box means. No index can serve a leading wildcard, so the
// query walks the account's entries - which is why the caller always passes a
// limit, and why scoping it to a subtree matters.
func SearchNodes(ctx context.Context, q Querier, userID int64, scope, pattern string, limit int) ([]Node, error) {
	like := pattern

	// Scoped with a range over the path index where possible, so searching
	// inside a folder does not read the whole account.
	if scope != "." && scope != "" {
		lo, hi, ok := PrefixRange(scope + "/")
		if !ok {
			return nil, nil
		}
		rows, err := q.QueryContext(ctx, `
			SELECT `+nodeColumns+` FROM nodes
			WHERE user_id = ? AND path >= ? AND path < ? AND name LIKE ? ESCAPE '\'
			ORDER BY path
			LIMIT ?`, userID, lo, hi, like, limit)
		if err != nil {
			return nil, err
		}
		return collectNodes(rows)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT `+nodeColumns+` FROM nodes
		WHERE user_id = ? AND path <> '.' AND name LIKE ? ESCAPE '\'
		ORDER BY path
		LIMIT ?`, userID, like, limit)
	if err != nil {
		return nil, err
	}
	return collectNodes(rows)
}
