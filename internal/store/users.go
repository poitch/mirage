package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/poitch/mirage/internal/account"
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

// otherMappings returns every account's identity and home except one.
func otherMappings(ctx context.Context, q Querier, excludeID int64) ([]account.Mapping, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT username, home FROM users WHERE id <> ?`, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []account.Mapping
	for rows.Next() {
		var m account.Mapping
		if err := rows.Scan(&m.Username, &m.Home); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// validateMapping applies the account rules and returns the cleaned mapping.
func validateMapping(ctx context.Context, q Querier, m UserMapping, excludeID int64) (UserMapping, error) {
	if err := account.ValidateUsername(m.Username); err != nil {
		return m, err
	}
	home, err := account.ValidateHome(m.Home)
	if err != nil {
		return m, err
	}
	m.Home = home
	if err := account.ValidateOwnership(m.UID, m.GID); err != nil {
		return m, err
	}
	if m.Quota < 0 {
		return m, errors.New("quota must not be negative (use 0 for unlimited)")
	}
	if m.DisplayName == "" {
		m.DisplayName = m.Username
	}

	others, err := otherMappings(ctx, q, excludeID)
	if err != nil {
		return m, err
	}
	if err := account.CheckConflicts(account.Mapping{Username: m.Username, Home: m.Home}, others); err != nil {
		return m, err
	}
	return m, nil
}

// CreateUser adds an account.
//
// Validation runs inside the transaction that inserts the row, so two admins
// adding overlapping accounts at once cannot both pass the conflict check and
// leave one account's directory sitting inside another's.
func (db *DB) CreateUser(ctx context.Context, m UserMapping) (User, error) {
	var created User
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		clean, err := validateMapping(ctx, tx, m, 0)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users (username, display_name, home, uid, gid, quota, disabled, disabled_reason, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
			clean.Username, clean.DisplayName, clean.Home, clean.UID, clean.GID, clean.Quota, now, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		created, err = scanUser(tx.QueryRowContext(ctx,
			`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
		return err
	})
	return created, err
}

// UpdateUser changes an account's identity and storage mapping.
//
// Credentials are untouched, as is the disabled state; those are separate
// actions with their own consequences.
func (db *DB) UpdateUser(ctx context.Context, id int64, m UserMapping) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		before, err := scanUser(tx.QueryRowContext(ctx,
			`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
		if err != nil {
			return err
		}
		clean, err := validateMapping(ctx, tx, m, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE users SET username = ?, display_name = ?, home = ?, uid = ?, gid = ?,
			                 quota = ?, updated_at = ?
			WHERE id = ?`,
			clean.Username, clean.DisplayName, clean.Home, clean.UID, clean.GID,
			clean.Quota, time.Now().Unix(), id); err != nil {
			return err
		}
		// A moved home invalidates every indexed path for this account: the
		// stored file IDs and ETags describe a tree Mirage no longer reads.
		if before.Home != clean.Home {
			if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE user_id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteUser removes an account, its credentials and its index.
//
// The files themselves are left alone. They are ordinary files on the NAS that
// exist independently of Mirage, and deleting somebody's documents because an
// account was removed is not a decision this should make.
func (db *DB) DeleteUser(ctx context.Context, id int64) error {
	res, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
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
