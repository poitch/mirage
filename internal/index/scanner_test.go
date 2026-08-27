package index

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poitch/mirage/internal/fsx"
	"github.com/poitch/mirage/internal/store"
)

type fixture struct {
	scanner *Scanner
	db      *store.DB
	user    store.User
	home    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	home := filepath.Join(t.TempDir(), "alice")
	mustMkdirAll(t, filepath.Join(home, "docs", "nested"))
	mustWrite(t, filepath.Join(home, "top.txt"), "top level")
	mustWrite(t, filepath.Join(home, "docs", "report.txt"), "report")
	mustWrite(t, filepath.Join(home, "docs", "nested", "deep.txt"), "deep")

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ReconcileUsers(ctx, []store.UserMapping{
		{Username: "alice", Home: home, UID: os.Getuid(), GID: os.Getgid()},
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	user, _ := db.UserByName(ctx, "alice")

	mgr := fsx.NewManager(0o640, 0o750)
	t.Cleanup(func() { mgr.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &fixture{scanner: NewScanner(db, mgr, log), db: db, user: user, home: home}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f *fixture) scan(t *testing.T) Stats {
	t.Helper()
	stats, err := f.scanner.ScanUser(context.Background(), f.user)
	if err != nil {
		t.Fatalf("ScanUser: %v", err)
	}
	return stats
}

func (f *fixture) node(t *testing.T, path string) store.Node {
	t.Helper()
	n, err := store.NodeByPath(context.Background(), f.db, f.user.ID, path)
	if err != nil {
		t.Fatalf("node %q: %v", path, err)
	}
	return n
}

func TestScanIndexesTree(t *testing.T) {
	f := newFixture(t)
	stats := f.scan(t)

	if stats.Files != 3 {
		t.Errorf("Files = %d, want 3", stats.Files)
	}
	if stats.Dirs != 2 {
		t.Errorf("Dirs = %d, want 2", stats.Dirs)
	}

	root := f.node(t, ".")
	if !root.IsDir {
		t.Error("root should be a directory")
	}
	// A directory's size is the total of everything beneath it, which is what
	// clients show and what quota accounting reads.
	wantBytes := int64(len("top level") + len("report") + len("deep"))
	if root.Size != wantBytes {
		t.Errorf("root size = %d, want %d", root.Size, wantBytes)
	}

	deep := f.node(t, "docs/nested/deep.txt")
	if deep.IsDir || deep.Size != 4 {
		t.Errorf("deep.txt: isDir=%v size=%d, want file of 4 bytes", deep.IsDir, deep.Size)
	}
	if deep.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("content type = %q, want text/plain", deep.ContentType)
	}
}

// TestRescanIsIdempotent matters because a periodic rescan runs forever. If it
// produced new ETags each time, every client would resynchronise on every pass.
func TestRescanIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, ".")
	beforeID := before.ID

	f.scan(t)
	after := f.node(t, ".")

	if after.ETag != before.ETag {
		t.Errorf("root ETag changed across an unchanged rescan: %q then %q", before.ETag, after.ETag)
	}
	if after.ID != beforeID {
		t.Errorf("root file ID changed across rescan: %d then %d", beforeID, after.ID)
	}
}

// TestChangePropagatesToRoot is the sync-critical property: a client that sees
// an unchanged root ETag never looks any deeper, so an edit three levels down
// has to reach the top.
func TestChangePropagatesToRoot(t *testing.T) {
	f := newFixture(t)
	f.scan(t)

	rootBefore := f.node(t, ".").ETag
	docsBefore := f.node(t, "docs").ETag
	nestedBefore := f.node(t, "docs/nested").ETag

	mustWrite(t, filepath.Join(f.home, "docs", "nested", "deep.txt"), "deep content, now longer")
	f.scan(t)

	if f.node(t, "docs/nested").ETag == nestedBefore {
		t.Error("containing directory ETag did not change")
	}
	if f.node(t, "docs").ETag == docsBefore {
		t.Error("intermediate directory ETag did not change")
	}
	if f.node(t, ".").ETag == rootBefore {
		t.Error("root ETag did not change; clients would never see the edit")
	}
}

func TestScanNoticesAddsAndDeletes(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	rootBefore := f.node(t, ".").ETag

	mustWrite(t, filepath.Join(f.home, "docs", "added.txt"), "new file")
	f.scan(t)
	if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, "docs/added.txt"); err != nil {
		t.Fatalf("added file not indexed: %v", err)
	}
	rootAfterAdd := f.node(t, ".").ETag
	if rootAfterAdd == rootBefore {
		t.Error("root ETag unchanged after adding a file")
	}

	if err := os.Remove(filepath.Join(f.home, "docs", "added.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	f.scan(t)
	if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, "docs/added.txt"); err == nil {
		t.Error("deleted file is still indexed")
	}
	// Removing the file restores the tree to its original shape, so the ETag
	// should return to its original value too.
	if f.node(t, ".").ETag != rootBefore {
		t.Error("root ETag did not return to its original value after the file was removed again")
	}
}

func TestScanRemovesDeletedDirectory(t *testing.T) {
	f := newFixture(t)
	f.scan(t)

	if err := os.RemoveAll(filepath.Join(f.home, "docs")); err != nil {
		t.Fatalf("remove docs: %v", err)
	}
	f.scan(t)

	for _, p := range []string{"docs", "docs/report.txt", "docs/nested", "docs/nested/deep.txt"} {
		if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, p); err == nil {
			t.Errorf("%q survived deletion of its parent directory", p)
		}
	}
}

// TestFileIDsSurviveRescan protects the identity clients use to tell a rename
// from a delete-plus-create.
func TestFileIDsSurviveRescan(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, "docs/report.txt").ID

	mustWrite(t, filepath.Join(f.home, "docs", "report.txt"), "edited report content")
	f.scan(t)

	after := f.node(t, "docs/report.txt")
	if after.ID != before {
		t.Errorf("file ID changed on edit: %d then %d", before, after.ID)
	}
	if after.ETag == "" {
		t.Error("edited file has no ETag")
	}
}

// TestUnreadableDirectoryIsNotTreatedAsEmpty is the dangerous case. If a
// directory that cannot be read were scanned as empty, the scan would delete
// its indexed contents and every client would then delete the real files.
func TestUnreadableDirectoryIsNotTreatedAsEmpty(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not restrict access")
	}
	f := newFixture(t)
	f.scan(t)

	docs := filepath.Join(f.home, "docs")
	if err := os.Chmod(docs, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(docs, 0o755) })

	stats := f.scan(t)
	if stats.Skipped == 0 {
		t.Error("scan did not report skipping the unreadable directory")
	}

	for _, p := range []string{"docs/report.txt", "docs/nested/deep.txt"} {
		if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, p); err != nil {
			t.Errorf("%q was dropped from the index because its parent could not be read: %v", p, err)
		}
	}
}

func TestUserUsageTracksScan(t *testing.T) {
	f := newFixture(t)
	f.scan(t)

	used, err := store.UserUsage(context.Background(), f.db, f.user.ID)
	if err != nil {
		t.Fatalf("UserUsage: %v", err)
	}
	want := int64(len("top level") + len("report") + len("deep"))
	if used != want {
		t.Errorf("usage = %d, want %d", used, want)
	}
}

// TestScanDetectsRename is what stops a folder rename over SMB from looking
// like a mass delete. Clients tell a rename from a delete-plus-create by the
// file ID, so it has to survive the move.
func TestScanDetectsRename(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, "docs/report.txt")

	if err := os.Rename(
		filepath.Join(f.home, "docs", "report.txt"),
		filepath.Join(f.home, "docs", "renamed.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	stats := f.scan(t)

	if stats.Moved != 1 {
		t.Errorf("Moved = %d, want 1", stats.Moved)
	}
	after := f.node(t, "docs/renamed.txt")
	if after.ID != before.ID {
		t.Errorf("file ID changed across an out-of-band rename: %d then %d", before.ID, after.ID)
	}
	if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, "docs/report.txt"); err == nil {
		t.Error("the old path is still indexed")
	}
}

// TestScanDetectsRenameAcrossDirectories covers the ordering trap. Pruning per
// directory would make detection depend on whether the source or destination
// happened to be walked first; sweeping once at the end removes that.
func TestScanDetectsRenameAcrossDirectories(t *testing.T) {
	// Both directions: destination sorts before the source, and after it.
	tests := []struct{ name, from, to string }{
		{"into a later directory", "top.txt", "docs/moved.txt"},
		{"into an earlier directory", "docs/nested/deep.txt", "aaa-moved.txt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.scan(t)
			before := f.node(t, tc.from)

			if err := os.Rename(filepath.Join(f.home, tc.from), filepath.Join(f.home, tc.to)); err != nil {
				t.Fatalf("rename: %v", err)
			}
			f.scan(t)

			after := f.node(t, tc.to)
			if after.ID != before.ID {
				t.Errorf("file ID changed moving %s -> %s: %d then %d",
					tc.from, tc.to, before.ID, after.ID)
			}
		})
	}
}

// TestScanDoesNotMistakeHardLinkForRename: a hard link puts one inode at two
// live paths. Taking the file ID from the original would corrupt the entry that
// legitimately holds it.
func TestScanDoesNotMistakeHardLinkForRename(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	original := f.node(t, "top.txt")

	if err := os.Link(filepath.Join(f.home, "top.txt"), filepath.Join(f.home, "linked.txt")); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	stats := f.scan(t)

	if stats.Moved != 0 {
		t.Errorf("Moved = %d, want 0: a hard link is not a rename", stats.Moved)
	}
	if f.node(t, "top.txt").ID != original.ID {
		t.Error("the original file lost its ID to its hard link")
	}
	if f.node(t, "linked.txt").ID == original.ID {
		t.Error("a hard link was given the same file ID as the original")
	}
}

// TestUnreadableDirectorySurvivesTheSweep re-checks the M2 safety property
// against the end-of-scan sweep, which is a second way the same disaster could
// happen: a subtree that could not be read must not be judged missing.
func TestUnreadableDirectorySurvivesTheSweep(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits do not restrict access")
	}
	f := newFixture(t)
	f.scan(t)

	docs := filepath.Join(f.home, "docs")
	if err := os.Chmod(docs, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(docs, 0o755) })

	stats := f.scan(t)
	if stats.Removed != 0 {
		t.Errorf("Removed = %d; an unreadable directory must not cause deletions", stats.Removed)
	}
	for _, p := range []string{"docs", "docs/report.txt", "docs/nested", "docs/nested/deep.txt"} {
		if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, p); err != nil {
			t.Errorf("%q was swept because its parent could not be read: %v", p, err)
		}
	}
}

func TestScanReportsRemovals(t *testing.T) {
	f := newFixture(t)
	f.scan(t)

	if err := os.Remove(filepath.Join(f.home, "top.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	stats := f.scan(t)
	if stats.Removed != 1 {
		t.Errorf("Removed = %d, want 1", stats.Removed)
	}
}

// TestSymlinksAreSkippedButReported covers the failure that is worst when
// silent: a folder that exists on the NAS but never appears on any client. On a
// Synology, home directories routinely link to shared folders elsewhere.
func TestSymlinksAreSkippedButReported(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "shared.txt"), "in a shared folder")

	if err := os.Symlink(outside, filepath.Join(f.home, "Music")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	stats := f.scan(t)

	if stats.SkippedLinks != 1 {
		t.Errorf("SkippedLinks = %d, want 1; a skipped link must be counted, not dropped quietly",
			stats.SkippedLinks)
	}
	if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, "Music"); err == nil {
		t.Error("a symlink was indexed; it may point outside the account's directory")
	}
}

// TestSynologyMetadataIsIgnored: @eaDir appears in every directory on a
// Synology volume and holds thumbnails, not user files.
func TestSynologyMetadataIsIgnored(t *testing.T) {
	f := newFixture(t)
	mustMkdirAll(t, filepath.Join(f.home, "docs", "@eaDir"))
	mustWrite(t, filepath.Join(f.home, "docs", "@eaDir", "report.txt@SynoThumb"), "thumbnail")
	mustMkdirAll(t, filepath.Join(f.home, "#recycle"))
	mustWrite(t, filepath.Join(f.home, "#recycle", "deleted.txt"), "in the bin")

	f.scan(t)
	for _, p := range []string{"docs/@eaDir", "docs/@eaDir/report.txt@SynoThumb", "#recycle", "#recycle/deleted.txt"} {
		if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, p); err == nil {
			t.Errorf("%q was indexed; filesystem machinery must not sync", p)
		}
	}
	// The real files alongside them are unaffected.
	if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, "docs/report.txt"); err != nil {
		t.Errorf("ignoring metadata also hid a real file: %v", err)
	}
}

// TestDirectoryNeverReportsEpochZero: a directory's ETag can only be computed
// after its children are read, so between being created and being finalised the
// row is visible to clients. Column defaults would show 1970 and an empty ETag.
func TestDirectoryNeverReportsEpochZero(t *testing.T) {
	f := newFixture(t)
	f.scan(t)

	for _, p := range []string{".", "docs", "docs/nested"} {
		n := f.node(t, p)
		if n.MTime.Unix() <= 0 {
			t.Errorf("%q has modification time %v; clients render that as 1970", p, n.MTime)
		}
		if n.ETag == "" {
			t.Errorf("%q has an empty ETag, which is not a valid ETag", p)
		}
	}
}

// TestInterruptedScanLeavesNoEpochZeroRows covers what a scan that was stopped
// partway leaves behind. Restarting the server begins the walk again, and until
// it reaches a given directory that directory's row is whatever the previous,
// unfinished pass created - which must not be 1970 with an empty ETag.
func TestInterruptedScanLeavesNoEpochZeroRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Stand in for an interrupted scan: rows created but never finalised, which
	// is exactly what EnsureDirNode leaves before its children are read.
	f.scan(t)
	if _, err := f.db.ExecContext(ctx,
		`UPDATE nodes SET etag = '', mtime = 0 WHERE user_id = ? AND is_dir = 1`,
		f.user.ID); err != nil {
		t.Fatalf("simulate interrupted scan: %v", err)
	}

	// A fresh pass must repair them rather than preserve the provisional state.
	f.scan(t)
	for _, p := range []string{".", "docs", "docs/nested"} {
		n := f.node(t, p)
		if n.MTime.Unix() <= 0 {
			t.Errorf("%q still reports the epoch after a rescan", p)
		}
		if n.ETag == "" {
			t.Errorf("%q still has an empty ETag after a rescan", p)
		}
	}
}

// TestInterruptedScanResumes is the property that matters on a large share: a
// scan stopped partway must pick up where it left off rather than repeat an
// hour of work, and must still produce exactly the index a single
// uninterrupted pass would have.
func TestInterruptedScanResumes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// A complete pass, to know what the answer should be.
	f.scan(t)
	want := map[string]string{}
	for _, p := range []string{".", "docs", "docs/nested", "top.txt", "docs/report.txt", "docs/nested/deep.txt"} {
		want[p] = f.node(t, p).ETag
	}

	// Simulate a scan interrupted after finishing docs/nested but nothing else.
	// A real scan stamps a directory's contents before finishing the directory,
	// so the subtree is stamped here too.
	stamp := store.Stamp()
	if _, err := f.db.ExecContext(ctx,
		`UPDATE nodes SET complete = 0, scanned_at = 0 WHERE user_id = ?`, f.user.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, p := range []string{"docs/nested", "docs/nested/deep.txt"} {
		n := f.node(t, p)
		if _, err := f.db.ExecContext(ctx,
			`UPDATE nodes SET complete = 1, scanned_at = ? WHERE id = ?`, stamp, n.ID); err != nil {
			t.Fatalf("mark %s: %v", p, err)
		}
	}
	writeProgress(t, f, Progress{
		User: f.user.Username, State: StateInterrupted, Stamp: stamp,
		StartedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute),
	})

	stats, err := f.scanner.ScanUser(ctx, f.user)
	if err != nil {
		t.Fatalf("resumed scan: %v", err)
	}
	if stats.Resumed != 1 {
		t.Errorf("Resumed = %d, want 1: the finished subtree should have been skipped", stats.Resumed)
	}

	// The resumed pass must arrive at the same index as an uninterrupted one.
	for p, etag := range want {
		if got := f.node(t, p).ETag; got != etag {
			t.Errorf("%q: ETag after resume = %q, want %q", p, got, etag)
		}
	}
	// And it must have completed, rather than leaving the record interrupted.
	final, _, err := ScanProgress(ctx, f.db)
	if err != nil {
		t.Fatalf("ScanProgress: %v", err)
	}
	if final.State != StateDone {
		t.Errorf("State = %q after a resumed scan, want %q", final.State, StateDone)
	}
}

// TestResumeDoesNotSweepUnvisitedEntries guards the dangerous interaction: the
// end-of-scan sweep deletes anything not carrying the current generation mark,
// and a resumed scan deliberately does not revisit completed subtrees.
//
// The directory alone is marked complete here, with its contents left unmarked.
// A real scan would have stamped them, so this is a state that should not
// arise - which is the point: if it ever did, entries would vanish from the
// index and clients would delete the files. Resuming re-stamps the subtree
// rather than trusting that.
func TestResumeDoesNotSweepUnvisitedEntries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.scan(t)

	stamp := store.Stamp()
	if _, err := f.db.ExecContext(ctx,
		`UPDATE nodes SET complete = 0, scanned_at = 0 WHERE user_id = ?`, f.user.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, p := range []string{"docs/nested"} {
		n := f.node(t, p)
		if _, err := f.db.ExecContext(ctx,
			`UPDATE nodes SET complete = 1, scanned_at = ? WHERE id = ?`, stamp, n.ID); err != nil {
			t.Fatalf("mark %s: %v", p, err)
		}
	}
	writeProgress(t, f, Progress{
		User: f.user.Username, State: StateInterrupted, Stamp: stamp,
		StartedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute),
	})

	if _, err := f.scanner.ScanUser(ctx, f.user); err != nil {
		t.Fatalf("resumed scan: %v", err)
	}
	// The file inside the skipped subtree was never revisited, and must not
	// have been swept as though it had vanished from disk.
	if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "docs/nested/deep.txt"); err != nil {
		t.Fatalf("a resumed scan swept an entry inside a completed subtree: %v", err)
	}
}
