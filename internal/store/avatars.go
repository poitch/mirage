package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Avatar is an uploaded account picture.
type Avatar struct {
	Image     []byte
	UpdatedAt time.Time
}

// SetAvatar stores an account's picture, replacing any previous one.
func (db *DB) SetAvatar(ctx context.Context, userID int64, image []byte) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO avatars (user_id, image, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET image = excluded.image, updated_at = excluded.updated_at`,
		userID, image, time.Now().UnixNano())
	return err
}

// ClearAvatar removes an account's picture, so it falls back to a generated one.
func (db *DB) ClearAvatar(ctx context.Context, userID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM avatars WHERE user_id = ?`, userID)
	return err
}

// AvatarFor returns an account's uploaded picture, or ErrNotFound if it has
// none and should be given a generated one.
func (db *DB) AvatarFor(ctx context.Context, userID int64) (Avatar, error) {
	var a Avatar
	var nanos int64
	err := db.QueryRowContext(ctx,
		`SELECT image, updated_at FROM avatars WHERE user_id = ?`, userID).Scan(&a.Image, &nanos)
	if errors.Is(err, sql.ErrNoRows) {
		return Avatar{}, ErrNotFound
	}
	if err != nil {
		return Avatar{}, err
	}
	a.UpdatedAt = time.Unix(0, nanos)
	return a, nil
}

// AvatarVersion reports when an account's picture last changed, or zero if it
// has none.
//
// Separate from AvatarFor so that answering a conditional request does not read
// the image out of the database only to discard it - which is most requests,
// because clients ask far more often than a picture changes.
func (db *DB) AvatarVersion(ctx context.Context, userID int64) (time.Time, error) {
	var nanos int64
	err := db.QueryRowContext(ctx,
		`SELECT updated_at FROM avatars WHERE user_id = ?`, userID).Scan(&nanos)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, nanos), nil
}
