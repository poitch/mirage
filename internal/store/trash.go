package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TrashEntry is a deleted file, kept until it is restored or expires.
type TrashEntry struct {
	ID int64
	// Name is what the file is stored under inside the trash directory, and is
	// also how clients address it.
	Name string
	// OriginalPath is where it was, which is where restoring puts it back.
	OriginalPath string
	DeletedAt    time.Time
	Size         int64
	IsDir        bool
}

// AddTrashEntry records a deletion.
func AddTrashEntry(ctx context.Context, q Querier, userID int64, e TrashEntry) (int64, error) {
	res, err := q.ExecContext(ctx, `
		INSERT INTO trash (user_id, trash_name, original_path, deleted_at, size, is_dir)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, e.Name, e.OriginalPath, e.DeletedAt.Unix(), e.Size, boolInt(e.IsDir))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const trashColumns = `id, trash_name, original_path, deleted_at, size, is_dir`

func scanTrash(row interface{ Scan(...any) error }) (TrashEntry, error) {
	var e TrashEntry
	var deletedAt int64
	var isDir int
	if err := row.Scan(&e.ID, &e.Name, &e.OriginalPath, &deletedAt, &e.Size, &isDir); err != nil {
		return TrashEntry{}, err
	}
	e.DeletedAt = time.Unix(deletedAt, 0)
	e.IsDir = isDir != 0
	return e, nil
}

// ListTrash returns an account's deleted files, most recent first.
func ListTrash(ctx context.Context, q Querier, userID int64) ([]TrashEntry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+trashColumns+` FROM trash WHERE user_id = ? ORDER BY deleted_at DESC, id DESC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TrashEntry
	for rows.Next() {
		e, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrashByName finds one entry. Scoped to the account, so another account's
// entry is simply not found.
func TrashByName(ctx context.Context, q Querier, userID int64, name string) (TrashEntry, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+trashColumns+` FROM trash WHERE user_id = ? AND trash_name = ?`, userID, name)
	e, err := scanTrash(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TrashEntry{}, ErrNotFound
	}
	return e, err
}

// RemoveTrashEntry forgets a deletion, either because it was restored or
// because it was emptied.
func RemoveTrashEntry(ctx context.Context, q Querier, userID int64, name string) error {
	_, err := q.ExecContext(ctx,
		`DELETE FROM trash WHERE user_id = ? AND trash_name = ?`, userID, name)
	return err
}

// ExpiredTrash returns entries deleted before cutoff.
func ExpiredTrash(ctx context.Context, q Querier, userID int64, cutoff time.Time) ([]TrashEntry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+trashColumns+` FROM trash WHERE user_id = ? AND deleted_at < ? ORDER BY deleted_at`,
		userID, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TrashEntry
	for rows.Next() {
		e, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrashUsage reports how much disk an account's trash is holding.
//
// Worth surfacing: trashed files still occupy the person's share of the volume,
// and somebody wondering where their space went deserves an answer.
func TrashUsage(ctx context.Context, q Querier, userID int64) (int64, int64, error) {
	var count, bytes sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(size) FROM trash WHERE user_id = ?`, userID).Scan(&count, &bytes)
	return count.Int64, bytes.Int64, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
