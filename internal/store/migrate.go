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
	// 4: mark directories whose contents were fully read.
	//
	// A directory row is written before its children are scanned, so its mere
	// presence says nothing about whether the subtree beneath it was finished.
	// Recording that explicitly lets an interrupted scan resume: a directory
	// already completed under the same scan generation can be skipped whole,
	// instead of the walk starting again from the top.
	`
ALTER TABLE nodes ADD COLUMN complete INTEGER NOT NULL DEFAULT 0;
`,
	// 5: sub-second precision for modification times.
	//
	// The quick pass decides a directory changed by comparing its recorded
	// timestamp with the one on disk. At whole-second resolution a change
	// landing in the same second as the previously recorded one compares equal
	// and is missed. Rows written before this exist with zero here, which is
	// read as "unknown" and falls back to comparing seconds - so upgrading does
	// not make every directory look changed at once.
	`
ALTER TABLE nodes ADD COLUMN mtime_nsec INTEGER NOT NULL DEFAULT 0;
`,
	// 6: find the most recently changed directories cheaply.
	//
	// The kernel allows only so many filesystem watches, far fewer than a large
	// share has directories, so they have to be spent on the directories most
	// likely to change rather than on whichever the tree walk reached first.
	`
CREATE INDEX idx_nodes_dir_mtime ON nodes(user_id, is_dir, mtime DESC);
`,
	// 7: uploaded account pictures.
	//
	// Held here rather than in the account's own directory: anything placed
	// there is a file the account owns and syncs to every device, and a picture
	// the admin page put there would look to the person like something Mirage
	// had dumped in their files. One small row per account instead.
	`
CREATE TABLE avatars (
    user_id    INTEGER NOT NULL PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    image      BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
);
`,
	// 8: make searching by name find things without reading every row.
	//
	// A search box sends "contains this text", which is LIKE '%x%', and no
	// ordinary index can serve that - so every search read the whole account.
	// On a share of three million files that took seconds, and all of them
	// spent on the rows that do not match.
	//
	// A trigram index turns the problem around: it indexes every three-letter
	// run, so a substring becomes something that can be looked up. The table is
	// contentless - it stores the index but not another copy of the names - and
	// so answers MATCH only. A contentless table silently returns nothing for
	// LIKE, which is why the query treats MATCH as a way to narrow the
	// candidates and then checks them against the real column.
	//
	// Kept current by triggers rather than by the code that writes nodes. Nodes
	// are written from a dozen places and a search index that is only usually
	// updated gives wrong answers quietly, which is the worst way to be wrong.
	`
CREATE VIRTUAL TABLE node_names USING fts5(
    name,
    content='',
    contentless_delete=1,
    tokenize='trigram'
);

CREATE TRIGGER nodes_search_insert AFTER INSERT ON nodes BEGIN
    INSERT INTO node_names(rowid, name) VALUES (new.id, new.name);
END;

CREATE TRIGGER nodes_search_delete AFTER DELETE ON nodes BEGIN
    DELETE FROM node_names WHERE rowid = old.id;
END;

-- Guarded on the name actually changing. A rescan rewrites every row it
-- visits, and reindexing three million unchanged names on each pass would
-- cost far more than it is worth.
CREATE TRIGGER nodes_search_update AFTER UPDATE OF name ON nodes
WHEN old.name <> new.name BEGIN
    DELETE FROM node_names WHERE rowid = old.id;
    INSERT INTO node_names(rowid, name) VALUES (new.id, new.name);
END;

INSERT INTO node_names(rowid, name) SELECT id, name FROM nodes;
`,
	// 9: give photographs and videos their real content types.
	//
	// Go's built-in table has no entry for HEIC, and a container has no system
	// mime.types file to fall back on, so every iPhone photograph was indexed
	// as an anonymous blob. The media view in the mobile apps finds pictures by
	// searching for a content type beginning "image/", which meant it found
	// none of them.
	//
	// Corrected here rather than left to the next scan, because a scan only
	// rewrites entries whose file changed and these have not.
	`
UPDATE nodes SET content_type = 'image/heic'         WHERE lower(name) LIKE '%.heic';
UPDATE nodes SET content_type = 'image/heif'         WHERE lower(name) LIKE '%.heif';
UPDATE nodes SET content_type = 'image/heif'         WHERE lower(name) LIKE '%.hif';
UPDATE nodes SET content_type = 'image/avif'         WHERE lower(name) LIKE '%.avif';
UPDATE nodes SET content_type = 'image/webp'         WHERE lower(name) LIKE '%.webp';
UPDATE nodes SET content_type = 'image/jxl'          WHERE lower(name) LIKE '%.jxl';
UPDATE nodes SET content_type = 'image/x-adobe-dng'  WHERE lower(name) LIKE '%.dng';
UPDATE nodes SET content_type = 'image/x-canon-cr2'  WHERE lower(name) LIKE '%.cr2';
UPDATE nodes SET content_type = 'image/x-canon-cr3'  WHERE lower(name) LIKE '%.cr3';
UPDATE nodes SET content_type = 'image/x-nikon-nef'  WHERE lower(name) LIKE '%.nef';
UPDATE nodes SET content_type = 'image/x-sony-arw'   WHERE lower(name) LIKE '%.arw';
UPDATE nodes SET content_type = 'video/quicktime'    WHERE lower(name) LIKE '%.mov';
UPDATE nodes SET content_type = 'video/mp4'          WHERE lower(name) LIKE '%.mp4';
UPDATE nodes SET content_type = 'video/x-m4v'        WHERE lower(name) LIKE '%.m4v';
UPDATE nodes SET content_type = 'video/x-matroska'   WHERE lower(name) LIKE '%.mkv';
UPDATE nodes SET content_type = 'video/webm'         WHERE lower(name) LIKE '%.webm';
UPDATE nodes SET content_type = 'video/x-msvideo'    WHERE lower(name) LIKE '%.avi';
UPDATE nodes SET content_type = 'video/mp2t'         WHERE lower(name) LIKE '%.mts';
UPDATE nodes SET content_type = 'video/mp2t'         WHERE lower(name) LIKE '%.m2ts';

CREATE INDEX idx_nodes_content_type ON nodes(user_id, content_type);
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

	// Announced before it starts, not after. Most migrations are instant, but
	// one builds a search index over every indexed file, which on a large share
	// takes minutes - and a server that goes quiet during an upgrade is one
	// somebody restarts halfway through.
	// Only for an upgrade. A database being created has nothing to wait for and
	// the message would appear on every start of a fresh install.
	if current > 0 && current < len(migrations) && db.log != nil {
		db.log.Info("bringing the index database up to date",
			"from_version", current, "to_version", len(migrations),
			"note", "a large share can take several minutes; do not interrupt it")
	}
	for i := current; i < len(migrations); i++ {
		version := i + 1
		start := time.Now()
		if err := db.applyMigration(ctx, version, migrations[i]); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second && db.log != nil {
			db.log.Info("applied a database migration",
				"version", version, "duration", elapsed.Round(time.Millisecond))
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
