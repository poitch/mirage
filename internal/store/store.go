// Package store owns the Mirage index database.
//
// The database holds credentials plus a rebuildable index over the real
// filesystem. It is never the source of truth for file content: deleting it
// costs a full rescan, not data.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary cross-compiles for Synology
)

// DB wraps the index database.
type DB struct {
	*sql.DB
	path string
	// log reports the few things the store has to say for itself, which is
	// currently only that a slow migration is in progress. Defaulted rather
	// than injected so that every caller does not have to pass one.
	log *slog.Logger
}

// Open opens (creating if needed) the index database at path and brings its
// schema up to date.
func Open(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	// Pragmas go in the DSN so they are applied to every pooled connection.
	// foreign_keys in particular is per-connection, and setting it once after
	// Open would leave later connections without it.
	dsn := "file:" + url.PathEscape(path) + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	db := &DB{DB: sqlDB, path: path, log: slog.Default()}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return db, nil
}

// Path returns the on-disk location of the database.
func (db *DB) Path() string { return db.path }

// Tx runs fn inside a transaction, rolling back on error.
func (db *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
