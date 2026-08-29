package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Version is an earlier copy of a file.
type Version struct {
	ID     int64
	NodeID int64
	// Path is where the file was when this version was taken. Kept so a
	// listing can say what it was a version of even after the file moves.
	Path      string
	Timestamp time.Time
	Size      int64
}

// AddVersion records a stored version.
func AddVersion(ctx context.Context, q Querier, userID int64, v Version) (int64, error) {
	res, err := q.ExecContext(ctx, `
		INSERT INTO versions (user_id, node_id, path, timestamp, size, storage_name)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, v.NodeID, v.Path, v.Timestamp.Unix(), v.Size, "")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const versionColumns = `id, node_id, path, timestamp, size`

func scanVersion(row interface{ Scan(...any) error }) (Version, error) {
	var v Version
	var ts int64
	if err := row.Scan(&v.ID, &v.NodeID, &v.Path, &ts, &v.Size); err != nil {
		return Version{}, err
	}
	v.Timestamp = time.Unix(ts, 0)
	return v, nil
}

// VersionsOf returns a file's stored versions, newest first.
func VersionsOf(ctx context.Context, q Querier, userID, nodeID int64) ([]Version, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+versionColumns+` FROM versions
		WHERE user_id = ? AND node_id = ? ORDER BY timestamp DESC`, userID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectVersions(rows)
}

// VersionAt finds one version by its timestamp, which is how clients address
// it. Scoped to the account, so another account's version is not found.
func VersionAt(ctx context.Context, q Querier, userID, nodeID int64, ts time.Time) (Version, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+versionColumns+` FROM versions
		WHERE user_id = ? AND node_id = ? AND timestamp = ?`, userID, nodeID, ts.Unix())
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	return v, err
}

// RemoveVersion forgets one stored version.
func RemoveVersion(ctx context.Context, q Querier, userID, id int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM versions WHERE user_id = ? AND id = ?`, userID, id)
	return err
}

// RemoveVersionsOf forgets every version of a file.
func RemoveVersionsOf(ctx context.Context, q Querier, userID, nodeID int64) error {
	_, err := q.ExecContext(ctx,
		`DELETE FROM versions WHERE user_id = ? AND node_id = ?`, userID, nodeID)
	return err
}

// SurplusVersions returns the versions of a file beyond the most recent keep,
// oldest first.
//
// A file saved every few minutes all day would otherwise accumulate hundreds of
// copies of itself, and the oldest of those are the least likely to be wanted.
func SurplusVersions(ctx context.Context, q Querier, userID, nodeID int64, keep int) ([]Version, error) {
	if keep < 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT `+versionColumns+` FROM versions
		WHERE user_id = ? AND node_id = ?
		ORDER BY timestamp DESC
		LIMIT -1 OFFSET ?`, userID, nodeID, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectVersions(rows)
}

// ExpiredVersions returns versions older than cutoff.
func ExpiredVersions(ctx context.Context, q Querier, userID int64, cutoff time.Time) ([]Version, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+versionColumns+` FROM versions
		WHERE user_id = ? AND timestamp < ? ORDER BY timestamp`, userID, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectVersions(rows)
}

// OrphanedVersions returns versions whose file is no longer indexed.
//
// Versions outlive a delete only briefly: the file goes to the trash, and once
// that expires there is nothing left to restore them onto.
func OrphanedVersions(ctx context.Context, q Querier, userID int64) ([]Version, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT v.id, v.node_id, v.path, v.timestamp, v.size FROM versions v
		LEFT JOIN nodes n ON n.id = v.node_id
		WHERE v.user_id = ? AND n.id IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectVersions(rows)
}

// VersionUsage reports how much disk an account's versions occupy.
func VersionUsage(ctx context.Context, q Querier, userID int64) (int64, int64, error) {
	var count, bytes sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(size) FROM versions WHERE user_id = ?`, userID).Scan(&count, &bytes)
	return count.Int64, bytes.Int64, err
}

func collectVersions(rows *sql.Rows) ([]Version, error) {
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
