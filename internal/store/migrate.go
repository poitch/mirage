package store

import (
	"context"
	"fmt"
	"time"
)

// migrations are applied in order; each entry's index+1 is its version number.
// Migrations are append-only: never edit one that has shipped.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE,
    display_name  TEXT    NOT NULL,
    password_hash TEXT    NOT NULL DEFAULT '',
    home          TEXT    NOT NULL,
    uid           INTEGER NOT NULL,
    gid           INTEGER NOT NULL,
    quota         INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    -- Why the account is disabled: '' when enabled, 'config' when it vanished
    -- from the config file, 'admin' when an operator locked it. Reconciling
    -- config re-enables the former but never the latter, so an emergency lock
    -- is not undone by the next restart.
    disabled_reason TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

-- App passwords are the credential sync clients actually present, on every
-- single request. token_hash is SHA-256 rather than a slow KDF because the
-- token is a long random secret, not a user-chosen password.
CREATE TABLE app_passwords (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   BLOB    NOT NULL UNIQUE,
    name         TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
);
CREATE INDEX idx_app_passwords_user ON app_passwords(user_id);

-- Login Flow v2 pairing sessions. Short-lived; pruned on a timer.
CREATE TABLE login_flows (
    poll_token_hash BLOB    PRIMARY KEY,
    login_token     TEXT    NOT NULL UNIQUE,
    user_id         INTEGER REFERENCES users(id) ON DELETE CASCADE,
    app_password    TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    completed_at    INTEGER
);

-- nodes.id IS oc:fileid. It must stay stable across renames, so a move updates
-- path and parent_id and leaves id alone; clients rely on that to tell a rename
-- from a delete-plus-create.
CREATE TABLE nodes (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id    INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    is_dir       INTEGER NOT NULL,
    size         INTEGER NOT NULL DEFAULT 0,
    mtime        INTEGER NOT NULL DEFAULT 0,
    etag         TEXT    NOT NULL,
    content_type TEXT    NOT NULL DEFAULT '',
    dev          INTEGER NOT NULL DEFAULT 0,
    inode        INTEGER NOT NULL DEFAULT 0,
    scanned_at   INTEGER NOT NULL DEFAULT 0,
    UNIQUE(user_id, path)
);
CREATE INDEX idx_nodes_parent ON nodes(parent_id);
-- Supports out-of-band rename detection during a rescan: a file that vanished
-- from one path and reappeared at another with the same (dev, inode) is a move.
CREATE INDEX idx_nodes_inode ON nodes(user_id, dev, inode);

CREATE TABLE uploads (
    id           TEXT    NOT NULL,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    destination  TEXT    NOT NULL,
    total_length INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (user_id, id)
);

CREATE TABLE trash (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    trash_name    TEXT    NOT NULL,
    original_path TEXT    NOT NULL,
    deleted_at    INTEGER NOT NULL,
    size          INTEGER NOT NULL DEFAULT 0,
    is_dir        INTEGER NOT NULL,
    UNIQUE(user_id, trash_name)
);

-- node_id is deliberately not a foreign key: versions outlive the node they
-- came from, so a delete must not cascade them away.
CREATE TABLE versions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    node_id      INTEGER NOT NULL,
    path         TEXT    NOT NULL,
    timestamp    INTEGER NOT NULL,
    size         INTEGER NOT NULL DEFAULT 0,
    storage_name TEXT    NOT NULL,
    UNIQUE(node_id, timestamp)
);
CREATE INDEX idx_versions_user ON versions(user_id, node_id);

-- Change feed driving notify_push.
CREATE TABLE sync_log (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    path    TEXT    NOT NULL,
    node_id INTEGER,
    change  TEXT    NOT NULL,
    at      INTEGER NOT NULL
);
CREATE INDEX idx_sync_log_user_at ON sync_log(user_id, at);
`,
	// 2: remember which client started a pairing flow, so the app password it
	// receives can be named after the device rather than left anonymous.
	`
ALTER TABLE login_flows ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
`,
	// 3: server-wide settings. Currently just the instance ID, which forms part
	// of the oc:id clients use to identify a file, and so must stay stable for
	// the life of the installation.
	`
CREATE TABLE settings (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);
`,
}

// migrate applies any migrations the database has not yet seen.
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); upgrade Mirage",
			current, len(migrations))
	}

	for i := current; i < len(migrations); i++ {
		version := i + 1
		if err := db.applyMigration(ctx, version, migrations[i]); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
	}
	return nil
}

func (db *DB) applyMigration(ctx context.Context, version int, stmt string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		version, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
