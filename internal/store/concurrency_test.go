package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A scan and a filesystem watcher write at the same time, and on a share being
// filled over SMB they do so constantly. SQLite has one writer, so they
// contend - and the way they were failing was not a wait but an outright
// refusal, which dropped the change on the floor.
//
// A transaction that begins deferred starts as a reader and asks to become a
// writer when it first writes. If anything else has written since it read, that
// request fails immediately with SQLITE_BUSY_SNAPSHOT: there is nothing to wait
// for, because the snapshot it read from is already stale. busy_timeout does
// not cover it. Taking the write lock when the transaction opens turns the
// whole thing into an ordinary wait.

// TestConcurrentWritersDoNotLoseWork is the shape that failed in production: a
// scan writing a directory at a time while a watcher writes single files.
func TestConcurrentWritersDoNotLoseWork(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.ReconcileUsers(ctx, []UserMapping{{Username: "alice", Home: "/tmp/a"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u, _ := db.UserByName(ctx, "alice")

	const (
		writers = 8
		rounds  = 60
	)
	var (
		mu       sync.Mutex
		failures []error
	)
	fail := func(err error) {
		mu.Lock()
		failures = append(failures, err)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				stamp := Stamp()
				// Read before writing, which is what makes a deferred
				// transaction upgrade and what made this fail.
				err := db.Tx(ctx, func(tx *sql.Tx) error {
					path := fmt.Sprintf("d%d/f%d.txt", w, i)
					if _, err := NodeByPath(ctx, tx, u.ID, path); err != nil && err != ErrNotFound {
						return err
					}
					_, err := UpsertNode(ctx, tx, Node{
						UserID: u.ID, Path: path, Name: fmt.Sprintf("f%d.txt", i),
						Size: int64(i), ETag: "e", ContentType: "text/plain",
					}, stamp)
					return err
				})
				if err != nil {
					fail(err)
				}
			}
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		locked := 0
		for _, e := range failures {
			if isLocked(e) {
				locked++
			}
		}
		t.Fatalf("%d of %d transactions failed, %d of them on locking; first: %v",
			len(failures), writers*rounds, locked, failures[0])
	}

	// Every write landed, which is the point: a failure here is a file that
	// exists on disk and is invisible to every client until the next scan.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE user_id = ? AND is_dir = 0`, u.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != writers*rounds {
		t.Errorf("%d files indexed, want %d", count, writers*rounds)
	}
}

// TestWritersWaitRatherThanFail: a writer held up by a long transaction must
// queue behind it, not be refused.
func TestWritersWaitRatherThanFail(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.ReconcileUsers(ctx, []UserMapping{{Username: "alice", Home: "/tmp/a"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u, _ := db.UserByName(ctx, "alice")

	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := UpsertNode(ctx, tx, Node{
				UserID: u.ID, Path: "slow.txt", Name: "slow.txt", ETag: "e",
			}, Stamp()); err != nil {
				return err
			}
			close(held)
			// Long enough that a second writer certainly arrives during it.
			time.Sleep(250 * time.Millisecond)
			return nil
		})
	}()

	<-held
	start := time.Now()
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := UpsertNode(ctx, tx, Node{
			UserID: u.ID, Path: "quick.txt", Name: "quick.txt", ETag: "e",
		}, Stamp())
		return err
	})
	waited := time.Since(start)

	if err != nil {
		t.Fatalf("the second writer was refused rather than made to wait: %v", err)
	}
	if waited < 50*time.Millisecond {
		t.Errorf("the second writer returned in %s, so it cannot have waited for the first", waited)
	}
	if err := <-done; err != nil {
		t.Fatalf("the long transaction failed: %v", err)
	}
}

func isLocked(err error) bool {
	s := err.Error()
	return strings.Contains(s, "locked") || strings.Contains(s, "BUSY")
}

// TestBusyErrorsAreRecognised: the retry only fires for a lock, so a mistake in
// recognising one either retries real errors or gives up on contention.
func TestBusyErrorsAreRecognised(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// A statement error is not contention, and must be reported at once rather
	// than tried three times.
	start := time.Now()
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO nodes (nonexistent) VALUES (1)`)
		return err
	})
	if err == nil {
		t.Fatal("a broken statement succeeded")
	}
	if isBusy(err) {
		t.Errorf("a broken statement was mistaken for contention: %v", err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("a broken statement took %s, so it was retried", took)
	}
}
