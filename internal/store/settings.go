package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
)

const instanceIDKey = "instance_id"

// InstanceID returns this installation's identifier, generating it on first
// use.
//
// It forms the tail of every oc:id Mirage reports, so clients treat it as part
// of a file's identity. Changing it would make every file look new to every
// client, which is why it is persisted rather than derived at startup.
func (db *DB) InstanceID(ctx context.Context) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, instanceIDKey).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// Lower-case alphanumeric, matching the shape Nextcloud uses.
	id = strings.ToLower(rand.Text())[:10]
	// INSERT OR IGNORE plus a re-read, so two servers racing on the same
	// database still agree on one value.
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, instanceIDKey, id); err != nil {
		return "", err
	}
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, instanceIDKey).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}
