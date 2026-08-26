package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// User is an account as stored in the index database.
type User struct {
	ID           int64
	Username     string
	DisplayName  string
	PasswordHash string
	Home         string
	UID          int
	GID          int
	Quota        int64
	Disabled     bool
	// DisabledReason is "" when enabled, ReasonConfig when the account fell out
	// of the config file, or ReasonAdmin when an operator locked it.
	DisabledReason string
}

// Reasons an account can be disabled.
const (
	ReasonConfig = "config"
	ReasonAdmin  = "admin"
)

// UserMapping is the config-derived portion of a user: the identity and the
// filesystem location it maps onto. Credentials are not part of it, so
// reconciling from config never disturbs a password.
type UserMapping struct {
	Username    string
	DisplayName string
	Home        string
	UID         int
	GID         int
	Quota       int64
}

// ReconcileResult reports what ReconcileUsers changed.
type ReconcileResult struct {
	Created  []string
	Updated  []string
	Disabled []string
	// Reindex lists users whose home directory moved. Their cached index was
	// dropped and must be rebuilt by a scan before they can sync.
	Reindex []string
}

// ReconcileUsers makes the database agree with the user list from the config
// file, which is authoritative for identity and filesystem mapping.
//
// Users that disappear from the config are disabled rather than deleted: a
// delete would cascade away their app passwords, trash and version history, and
// a user vanishing from config is far more often an editing mistake than a
// deliberate erasure. Removing an account for real is an explicit CLI action.
//
// An account an operator locked with "mirage user disable" stays locked even
// though it is still listed in the config. Otherwise locking out a compromised
// account would silently undo itself at the next restart.
func (db *DB) ReconcileUsers(ctx context.Context, want []UserMapping) (ReconcileResult, error) {
	var res ReconcileResult
	now := time.Now().Unix()

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		existing := make(map[string]User)
		rows, err := tx.QueryContext(ctx,
			`SELECT id, username, display_name, home, uid, gid, quota, disabled, disabled_reason FROM users`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u User
			if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Home,
				&u.UID, &u.GID, &u.Quota, &u.Disabled, &u.DisabledReason); err != nil {
				return err
			}
			existing[u.Username] = u
		}
		if err := rows.Err(); err != nil {
			return err
		}

		wanted := make(map[string]bool, len(want))
		for _, w := range want {
			wanted[w.Username] = true
			old, ok := existing[w.Username]
			if !ok {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO users (username, display_name, home, uid, gid, quota, disabled, disabled_reason, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
					w.Username, w.DisplayName, w.Home, w.UID, w.GID, w.Quota, now, now); err != nil {
					return fmt.Errorf("create user %s: %w", w.Username, err)
				}
				res.Created = append(res.Created, w.Username)
				continue
			}

			// Presence in config clears a config-driven disable, but leaves an
			// operator's lock in place.
			disabled := old.Disabled && old.DisabledReason == ReasonAdmin
			reason := ""
			if disabled {
				reason = ReasonAdmin
			}

			unchanged := old.DisplayName == w.DisplayName && old.Home == w.Home &&
				old.UID == w.UID && old.GID == w.GID && old.Quota == w.Quota &&
				old.Disabled == disabled && old.DisabledReason == reason
			if unchanged {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE users SET display_name = ?, home = ?, uid = ?, gid = ?, quota = ?,
				                 disabled = ?, disabled_reason = ?, updated_at = ?
				WHERE id = ?`,
				w.DisplayName, w.Home, w.UID, w.GID, w.Quota, disabled, reason, now, old.ID); err != nil {
				return fmt.Errorf("update user %s: %w", w.Username, err)
			}
			res.Updated = append(res.Updated, w.Username)

			// A moved home invalidates every indexed path for this user. Drop
			// the index so the next scan rebuilds it rather than serving file
			// IDs and ETags that describe a directory tree we no longer read.
			if old.Home != w.Home {
				if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE user_id = ?`, old.ID); err != nil {
					return fmt.Errorf("clear index for %s: %w", w.Username, err)
				}
				res.Reindex = append(res.Reindex, w.Username)
			}
		}

		for name, u := range existing {
			if wanted[name] || u.Disabled {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE users SET disabled = 1, disabled_reason = ?, updated_at = ? WHERE id = ?`,
				ReasonConfig, now, u.ID); err != nil {
				return fmt.Errorf("disable user %s: %w", name, err)
			}
			res.Disabled = append(res.Disabled, name)
		}
		return nil
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	return res, nil
}

const userColumns = `id, username, display_name, password_hash, home, uid, gid, quota, disabled, disabled_reason`

// userColumnsPrefixed is userColumns qualified for use in a join, where a bare
// column list would be ambiguous.
const userColumnsPrefixed = `id, u.username, u.display_name, u.password_hash, u.home, u.uid, u.gid, u.quota, u.disabled, u.disabled_reason`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash,
		&u.Home, &u.UID, &u.GID, &u.Quota, &u.Disabled, &u.DisabledReason)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// UserByName looks up a user by username.
func (db *DB) UserByName(ctx context.Context, username string) (User, error) {
	return scanUser(db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = ?`, username))
}

// UserByID looks up a user by database ID.
func (db *DB) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// ListUsers returns every user, enabled or not, ordered by username.
func (db *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPasswordHash stores a new password hash for a user.
func (db *DB) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, time.Now().Unix(), userID)
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

// SetDisabled enables or disables a user as an explicit operator action. A
// disable made this way survives config reconciliation; see ReconcileUsers.
func (db *DB) SetDisabled(ctx context.Context, userID int64, disabled bool) error {
	reason := ""
	if disabled {
		reason = ReasonAdmin
	}
	_, err := db.ExecContext(ctx,
		`UPDATE users SET disabled = ?, disabled_reason = ?, updated_at = ? WHERE id = ?`,
		disabled, reason, time.Now().Unix(), userID)
	return err
}
