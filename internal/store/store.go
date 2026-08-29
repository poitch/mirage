// Package store owns the Mirage index database.
//
// The database holds credentials plus a rebuildable index over the real
// filesystem. It is never the source of truth for file content: deleting it
// costs a full rescan, not data.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite" // pure-Go driver: no cgo, so the binary cross-compiles for Synology
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
		"&_pragma=synchronous(NORMAL)" +
		// Every transaction takes the write lock when it opens, rather than
		// discovering it needs one partway through.
		//
		// The default is a deferred transaction, which begins as a reader and
		// upgrades if it writes. If anything else wrote in between, that
		// upgrade fails outright with SQLITE_BUSY_SNAPSHOT - and busy_timeout
		// does not help, because there is nothing to wait for: the snapshot the
		// transaction read from is already stale. Taking the lock up front
		// turns that into an ordinary wait, which the timeout does cover.
		"&_txlock=immediate"

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
//
// A transaction that cannot get the write lock is retried rather than reported.
// Transactions take that lock when they open and wait for it, so failing means
// the wait itself timed out - many seconds of unbroken contention, which
// happens when a scan and a bulk copy over SMB land together. A write that
// gives up is a file on disk that no client can see until the next scan finds
// it, so a few more attempts are worth their cost.
//
// Retrying is safe because a failed transaction leaves nothing behind: it is
// rolled back in full, and the next attempt runs against the state the first
// one saw.
func (db *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	var err error
	for attempt := range txAttempts {
		if attempt > 0 {
			// Backed off rather than retried at once, because what is in the
			// way is another writer that needs a moment to finish.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * txRetryDelay):
			}
		}
		if err = db.runTx(ctx, fn); err == nil || !isBusy(err) {
			return err
		}
	}
	return err
}

// txAttempts and txRetryDelay bound that retrying.
const (
	txAttempts   = 3
	txRetryDelay = 50 * time.Millisecond
)

// runTx is one attempt.
func (db *DB) runTx(ctx context.Context, fn func(*sql.Tx) error) error {
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

// The two result codes that mean somebody else holds the lock.
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// isBusy reports whether an error is SQLite declining to hand over the write
// lock, rather than something wrong with the statements.
func isBusy(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		// The primary result code is the low byte; the rest says why.
		switch serr.Code() & 0xFF {
		case sqliteBusy, sqliteLocked:
			return true
		}
	}
	return false
}
