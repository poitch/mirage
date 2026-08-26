package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AppPassword is a per-device credential.
type AppPassword struct {
	ID         int64
	UserID     int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// CreateAppPassword records a new app password for a user. Only the hash is
// stored; the caller is responsible for showing the token to the user once,
// because it can never be recovered afterwards.
func (db *DB) CreateAppPassword(ctx context.Context, userID int64, name string, tokenHash []byte) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO app_passwords (user_id, token_hash, name, created_at) VALUES (?, ?, ?, ?)`,
		userID, tokenHash, name, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UserByAppPassword resolves a token hash to its owner.
//
// Disabled accounts are excluded by the query rather than checked afterwards,
// so a disabled user's existing device tokens stop working immediately.
func (db *DB) UserByAppPassword(ctx context.Context, tokenHash []byte) (User, error) {
	row := db.QueryRowContext(ctx, `
		SELECT u.`+userColumnsPrefixed+`
		FROM app_passwords ap
		JOIN users u ON u.id = ap.user_id
		WHERE ap.token_hash = ? AND u.disabled = 0`, tokenHash)
	return scanUser(row)
}

// TouchAppPassword records that a token was just used. It is advisory, so
// callers may ignore its error rather than fail a request over it.
func (db *DB) TouchAppPassword(ctx context.Context, tokenHash []byte) error {
	_, err := db.ExecContext(ctx,
		`UPDATE app_passwords SET last_used_at = ? WHERE token_hash = ?`,
		time.Now().Unix(), tokenHash)
	return err
}

// DeleteAppPassword removes a single token. It reports ErrNotFound when the
// token does not exist or belongs to another user.
func (db *DB) DeleteAppPassword(ctx context.Context, userID int64, tokenHash []byte) error {
	res, err := db.ExecContext(ctx,
		`DELETE FROM app_passwords WHERE user_id = ? AND token_hash = ?`, userID, tokenHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAppPasswords returns a user's tokens, newest first.
func (db *DB) ListAppPasswords(ctx context.Context, userID int64) ([]AppPassword, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, name, created_at, last_used_at
		FROM app_passwords WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AppPassword
	for rows.Next() {
		var ap AppPassword
		var created int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&ap.ID, &ap.UserID, &ap.Name, &created, &lastUsed); err != nil {
			return nil, err
		}
		ap.CreatedAt = time.Unix(created, 0)
		if lastUsed.Valid {
			ap.LastUsedAt = time.Unix(lastUsed.Int64, 0)
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

// ErrNoRowsAffected signals an update that matched nothing.
var ErrNoRowsAffected = errors.New("no rows affected")
