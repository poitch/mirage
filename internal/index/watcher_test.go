package index

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poitch/mirage/internal/store"
)

// startWatcher runs a watcher against the fixture and returns once it is
// watching. The debounce is shortened so tests do not wait on the production
// coalescing window.
func (f *fixture) startWatcher(t *testing.T) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWatcher(f.db, f.scanner.storage, f.scanner, NewUpdater(f.db), log)
	w.debounce = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			t.Errorf("watcher stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Give the watcher a moment to register its watches before the test
	// changes anything, or the first event is simply missed.
	time.Sleep(150 * time.Millisecond)
}

// waitFor polls until cond holds or the deadline passes. The watcher is
// asynchronous, so a test cannot assert immediately after making a change.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *fixture) tryNode(path string) (store.Node, bool) {
	n, err := store.NodeByPath(context.Background(), f.db, f.user.ID, path)
	return n, err == nil
}

func (f *fixture) rootETag() string {
	n, err := store.NodeByPath(context.Background(), f.db, f.user.ID, ".")
	if err != nil {
		return ""
	}
	return n.ETag
}

// TestWatcherSeesNewFile is the point of M5: a file dropped in from outside
// Mirage - over SMB, or in File Station - reaches clients without waiting for
// the rescan interval.
func TestWatcherSeesNewFile(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	f.startWatcher(t)

	rootBefore := f.rootETag()
	mustWrite(t, filepath.Join(f.home, "docs", "nested", "dropped.txt"), "arrived out of band")

	waitFor(t, "the new file to be indexed", func() bool {
		_, ok := f.tryNode("docs/nested/dropped.txt")
		return ok
	})
	// Reaching the index is not enough: a client skips any directory whose
	// ETag it already knows, so the change has to surface at the root.
	waitFor(t, "the root ETag to change", func() bool {
		return f.rootETag() != rootBefore
	})
}

func TestWatcherSeesDeletion(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	f.startWatcher(t)

	if err := os.Remove(filepath.Join(f.home, "docs", "report.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	waitFor(t, "the deletion to be indexed", func() bool {
		_, ok := f.tryNode("docs/report.txt")
		return !ok
	})
}

func TestWatcherSeesModification(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, "docs/report.txt").ETag
	f.startWatcher(t)

	mustWrite(t, filepath.Join(f.home, "docs", "report.txt"), "substantially rewritten content")
	waitFor(t, "the edit to change the file's ETag", func() bool {
		n, ok := f.tryNode("docs/report.txt")
		return ok && n.ETag != before
	})
}

// TestWatcherSeesNewDirectory covers a directory that appears with files
// already inside it, which is what unpacking an archive or copying a folder
// over SMB looks like.
func TestWatcherSeesNewDirectory(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	f.startWatcher(t)

	dir := filepath.Join(f.home, "incoming")
	mustMkdirAll(t, filepath.Join(dir, "inner"))
	mustWrite(t, filepath.Join(dir, "a.txt"), "one")
	mustWrite(t, filepath.Join(dir, "inner", "b.txt"), "two")

	waitFor(t, "the new tree to be indexed", func() bool {
		_, a := f.tryNode("incoming/a.txt")
		_, b := f.tryNode("incoming/inner/b.txt")
		return a && b
	})
}

// TestWatcherPreservesFileIDOnRename: renaming over SMB must not make clients
// delete and re-download the file.
func TestWatcherPreservesFileIDOnRename(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, "docs/report.txt").ID
	f.startWatcher(t)

	if err := os.Rename(
		filepath.Join(f.home, "docs", "report.txt"),
		filepath.Join(f.home, "docs", "renamed-outside.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}

	waitFor(t, "the rename to be indexed", func() bool {
		_, ok := f.tryNode("docs/renamed-outside.txt")
		return ok
	})
	if after := f.node(t, "docs/renamed-outside.txt").ID; after != before {
		t.Errorf("file ID changed across a rename seen by the watcher: %d then %d", before, after)
	}
}

// TestWatcherIgnoresInternalPaths: the upload scratch area is inside the user's
// home, and a chunked upload writes to it constantly. Reacting to that would
// churn the index for files that are not there yet.
func TestWatcherIgnoresInternalPaths(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	f.startWatcher(t)

	rootBefore := f.rootETag()
	uploads := filepath.Join(f.home, ".mirage-uploads", "transfer-1")
	mustMkdirAll(t, uploads)
	mustWrite(t, filepath.Join(uploads, "1"), "a chunk in flight")

	// Give the watcher longer than its debounce to do the wrong thing.
	time.Sleep(300 * time.Millisecond)

	if _, ok := f.tryNode(".mirage-uploads"); ok {
		t.Error("the upload scratch area was indexed")
	}
	if f.rootETag() != rootBefore {
		t.Error("an in-flight chunk changed the root ETag, which would wake every client")
	}
}
