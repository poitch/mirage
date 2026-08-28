package index

import (
	"context"
	"errors"
	"fmt"
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

	mgr := fsx.NewManager(0o640, 0o750, nil)
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

// TestRescanSkipsUnchangedFilesButNoticesChanges is the correctness half of the
// rescan optimisation. Skipping unchanged files is only safe if a changed one
// is still noticed, and a miss here means a file that never syncs.
func TestRescanSkipsUnchangedFilesButNoticesChanges(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, "docs/report.txt").ETag
	rootBefore := f.node(t, ".").ETag

	// Nothing changed: every file should be recognised as such.
	stats := f.scan(t)
	if stats.Unchanged != 3 {
		t.Errorf("Unchanged = %d, want 3 (all files)", stats.Unchanged)
	}
	if f.node(t, ".").ETag != rootBefore {
		t.Error("an unchanged rescan altered the root ETag")
	}

	mustWrite(t, filepath.Join(f.home, "docs", "report.txt"), "edited, and a different length")
	stats = f.scan(t)
	if stats.Unchanged != 2 {
		t.Errorf("Unchanged = %d, want 2: the edited file must not be skipped", stats.Unchanged)
	}
	if f.node(t, "docs/report.txt").ETag == before {
		t.Error("the edited file kept its old ETag")
	}
	if f.node(t, ".").ETag == rootBefore {
		t.Error("the edit did not reach the root ETag")
	}
}

// TestRescanNoticesSameLengthEdit covers the case that made the comparison use
// ETags rather than timestamps: a file rewritten to the same length. The stored
// timestamp is only accurate to the second, so a rewrite inside one second
// would be invisible to a timestamp comparison.
func TestRescanNoticesSameLengthEdit(t *testing.T) {
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, "top.txt")

	// Same length, and a modification time deliberately inside the same second
	// as the original.
	mustWrite(t, filepath.Join(f.home, "top.txt"), "TOP LEVEL")
	if err := os.Chtimes(filepath.Join(f.home, "top.txt"),
		before.MTime, before.MTime.Add(400*time.Millisecond)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	f.scan(t)
	after := f.node(t, "top.txt")
	if after.ETag == before.ETag {
		t.Error("a same-length rewrite within one second was not noticed")
	}
	if after.ID != before.ID {
		t.Error("noticing the edit changed the file's ID")
	}
}

// TestScanHonoursExclusions covers the setting that makes a huge share
// tractable: whole subtrees of build output or version-control metadata can be
// left out of the index entirely.
func TestScanHonoursExclusions(t *testing.T) {
	f := newFixture(t)
	mustMkdirAll(t, filepath.Join(f.home, "code", ".svn", "text-base"))
	mustWrite(t, filepath.Join(f.home, "code", ".svn", "text-base", "a.svn-base"), "metadata")
	mustMkdirAll(t, filepath.Join(f.home, "code", "node_modules", "pkg"))
	mustWrite(t, filepath.Join(f.home, "code", "node_modules", "pkg", "index.js"), "code")
	mustWrite(t, filepath.Join(f.home, "code", "main.go"), "package main")

	excluder, err := fsx.NewExcluder([]string{".svn", "node_modules"})
	if err != nil {
		t.Fatalf("NewExcluder: %v", err)
	}
	mgr := fsx.NewManager(0o640, 0o750, excluder)
	t.Cleanup(func() { mgr.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanner := NewScanner(f.db, mgr, log)

	if _, err := scanner.ScanUser(context.Background(), f.user); err != nil {
		t.Fatalf("ScanUser: %v", err)
	}

	for _, p := range []string{"code/.svn", "code/.svn/text-base", "code/node_modules", "code/node_modules/pkg/index.js"} {
		if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, p); err == nil {
			t.Errorf("%q was indexed despite being excluded", p)
		}
	}
	if _, err := store.NodeByPath(context.Background(), f.db, f.user.ID, "code/main.go"); err != nil {
		t.Errorf("an excluded sibling hid a real file: %v", err)
	}
}

// TestQuickScanFindsNewFiles is the case that decides how long a file dropped
// over SMB takes to reach a client: a quick pass must notice it.
func TestQuickScanFindsNewFiles(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.scan(t)
	rootBefore := f.node(t, ".").ETag

	mustWrite(t, filepath.Join(f.home, "docs", "nested", "dropped-over-smb.txt"), "arrived")

	stats, err := f.scanner.QuickScanUser(ctx, f.user)
	if err != nil {
		t.Fatalf("QuickScanUser: %v", err)
	}
	if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "docs/nested/dropped-over-smb.txt"); err != nil {
		t.Fatalf("a quick pass missed a new file: %v", err)
	}
	// Reaching the index is not enough; a client skips a directory whose ETag
	// it already knows, so it has to surface at the root.
	if f.node(t, ".").ETag == rootBefore {
		t.Error("a quick pass did not propagate the new file to the root ETag")
	}
	// The point of the pass: only the directory that moved is re-read, not the
	// whole tree. Anything else and it would cost the same as a full scan.
	if stats.Changed != 1 {
		t.Errorf("Changed = %d, want 1: only the directory that moved should be re-read", stats.Changed)
	}
	if stats.Dirs < 3 {
		t.Errorf("Dirs = %d: the pass should still have checked every directory", stats.Dirs)
	}
}

func TestQuickScanFindsDeletionsAndRenames(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.scan(t)

	if err := os.Remove(filepath.Join(f.home, "docs", "report.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := f.scanner.QuickScanUser(ctx, f.user); err != nil {
		t.Fatalf("QuickScanUser: %v", err)
	}
	if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "docs/report.txt"); err == nil {
		t.Error("a quick pass missed a deletion")
	}

	before := f.node(t, "top.txt").ID
	if err := os.Rename(filepath.Join(f.home, "top.txt"), filepath.Join(f.home, "renamed.txt")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := f.scanner.QuickScanUser(ctx, f.user); err != nil {
		t.Fatalf("QuickScanUser: %v", err)
	}
	after, err := store.NodeByPath(ctx, f.db, f.user.ID, "renamed.txt")
	if err != nil {
		t.Fatalf("a quick pass missed a rename: %v", err)
	}
	if after.ID != before {
		t.Errorf("the rename lost the file's ID: %d then %d", before, after.ID)
	}
}

// TestQuickScanIsSoundWhenNothingChanged: it must not invent changes, or every
// client would resynchronise on every pass.
func TestQuickScanIsSoundWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, ".").ETag

	for range 3 {
		if _, err := f.scanner.QuickScanUser(ctx, f.user); err != nil {
			t.Fatalf("QuickScanUser: %v", err)
		}
	}
	if got := f.node(t, ".").ETag; got != before {
		t.Errorf("repeated quick passes changed the root ETag: %q then %q", before, got)
	}
	// And nothing was swept away by a pass that deliberately skips directories.
	for _, p := range []string{"top.txt", "docs/report.txt", "docs/nested/deep.txt"} {
		if _, err := store.NodeByPath(ctx, f.db, f.user.ID, p); err != nil {
			t.Errorf("a quick pass removed %q: %v", p, err)
		}
	}
}

// TestQuickScanMissesInPlaceEdits documents the one thing it cannot see, so the
// limitation is recorded rather than discovered. A file rewritten under the
// same name does not change its directory's timestamp; the full scan is what
// catches those.
func TestQuickScanMissesInPlaceEdits(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.scan(t)
	before := f.node(t, "docs/report.txt").ETag

	// Rewritten, then the directory's timestamp restored to what it was, which
	// is what an in-place write looks like on a real filesystem.
	docs := filepath.Join(f.home, "docs")
	dirInfo, err := os.Stat(docs)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mustWrite(t, filepath.Join(docs, "report.txt"), "quite different content here")
	if err := os.Chtimes(docs, dirInfo.ModTime(), dirInfo.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, err := f.scanner.QuickScanUser(ctx, f.user); err != nil {
		t.Fatalf("QuickScanUser: %v", err)
	}
	if f.node(t, "docs/report.txt").ETag != before {
		t.Skip("this filesystem surfaced the edit anyway; the limitation does not apply here")
	}

	// The full scan is the backstop, and must catch it.
	f.scan(t)
	if f.node(t, "docs/report.txt").ETag == before {
		t.Error("the full scan also missed an in-place edit")
	}
}

// TestStartupScanIsQuickWhenAlreadyIndexed: a restart must not cost a walk of
// the whole share, or restarting becomes something to avoid - and a server
// nobody dares restart is a worse problem than a slightly stale index.
func TestStartupScanIsQuickWhenAlreadyIndexed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Nothing indexed yet, so there is nothing to compare against and the full
	// walk is the only option.
	if err := f.scanner.StartupScan(ctx); err != nil {
		t.Fatalf("StartupScan on an empty index: %v", err)
	}
	if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "docs/nested/deep.txt"); err != nil {
		t.Fatalf("the first startup scan did not index the tree: %v", err)
	}

	// Something appears while the server is "down" - which is what a restart
	// has to catch, and what a quick pass can see.
	mustWrite(t, filepath.Join(f.home, "docs", "arrived-while-down.txt"), "new")
	rootBefore := f.node(t, ".").ETag

	if err := f.scanner.StartupScan(ctx); err != nil {
		t.Fatalf("StartupScan: %v", err)
	}
	if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "docs/arrived-while-down.txt"); err != nil {
		t.Errorf("a restart did not pick up a file added while the server was down: %v", err)
	}
	if f.node(t, ".").ETag == rootBefore {
		t.Error("the change did not reach the root ETag, so clients would not see it")
	}
}

// TestDirectoryRenameKeepsIDs covers what renaming a folder over SMB costs. A
// directory's entry has to travel with it, and so do the entries beneath it -
// otherwise a client sees one folder deleted and another created, and for a
// large folder that means discarding and re-fetching all of it.
func TestDirectoryRenameKeepsIDs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.scan(t)

	dirBefore := f.node(t, "docs").ID
	nestedBefore := f.node(t, "docs/nested").ID
	fileBefore := f.node(t, "docs/nested/deep.txt").ID

	if err := os.Rename(filepath.Join(f.home, "docs"), filepath.Join(f.home, "archive")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	stats := f.scan(t)
	if stats.Moved == 0 {
		t.Error("the rename was not recognised as one")
	}

	if got := f.node(t, "archive").ID; got != dirBefore {
		t.Errorf("directory id changed: %d then %d", dirBefore, got)
	}
	if got := f.node(t, "archive/nested").ID; got != nestedBefore {
		t.Errorf("nested directory id changed: %d then %d", nestedBefore, got)
	}
	if got := f.node(t, "archive/nested/deep.txt").ID; got != fileBefore {
		t.Errorf("file id inside the renamed directory changed: %d then %d", fileBefore, got)
	}
	if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "docs"); err == nil {
		t.Error("the old path is still indexed")
	}
}

// TestQuickScanKeepsIDsOnDirectoryRename: the quick pass is what actually sees
// a rename made over SMB, so it needs the same behaviour as the full scan.
func TestQuickScanKeepsIDsOnDirectoryRename(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.scan(t)

	dirBefore := f.node(t, "docs").ID
	fileBefore := f.node(t, "docs/nested/deep.txt").ID

	if err := os.Rename(filepath.Join(f.home, "docs"), filepath.Join(f.home, "archive")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := f.scanner.QuickScanUser(ctx, f.user); err != nil {
		t.Fatalf("QuickScanUser: %v", err)
	}

	after, err := store.NodeByPath(ctx, f.db, f.user.ID, "archive")
	if err != nil {
		t.Fatalf("renamed directory not indexed: %v", err)
	}
	if after.ID != dirBefore {
		t.Errorf("directory id changed: %d then %d", dirBefore, after.ID)
	}
	file, err := store.NodeByPath(ctx, f.db, f.user.ID, "archive/nested/deep.txt")
	if err != nil {
		t.Fatalf("file inside renamed directory not indexed: %v", err)
	}
	if file.ID != fileBefore {
		t.Errorf("file id changed: %d then %d", fileBefore, file.ID)
	}
}

// TestQuickScanParallelStatsFindEveryChange checks the concurrent pass across a
// tree wide enough that the work is spread over several workers, and with more
// than one directory changed at once.
//
// The serial version could not lose a change; the parallel one could, by
// dropping a result on a full channel or by racing on the shared list.
func TestQuickScanParallelStatsFindEveryChange(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	const dirs = 200
	for i := range dirs {
		mustMkdirAll(t, filepath.Join(f.home, fmt.Sprintf("d%03d", i)))
		mustWrite(t, filepath.Join(f.home, fmt.Sprintf("d%03d", i), "a.txt"), "a")
	}
	f.scan(t)

	// Changes spread across the tree, so a worker that quietly stopped early
	// would leave some of them unfound.
	changedDirs := []int{0, 17, 63, 128, 199}
	for _, i := range changedDirs {
		mustWrite(t, filepath.Join(f.home, fmt.Sprintf("d%03d", i), "new.txt"), "new")
	}

	stats, err := f.scanner.QuickScanUser(ctx, f.user)
	if err != nil {
		t.Fatalf("QuickScanUser: %v", err)
	}
	for _, i := range changedDirs {
		p := fmt.Sprintf("d%03d/new.txt", i)
		if _, err := store.NodeByPath(ctx, f.db, f.user.ID, p); err != nil {
			t.Errorf("the parallel pass missed %s: %v", p, err)
		}
	}
	if stats.Dirs < dirs {
		t.Errorf("Dirs = %d, want at least %d: some directories were never checked", stats.Dirs, dirs)
	}
	if stats.Changed != int64(len(changedDirs)) {
		t.Errorf("Changed = %d, want %d", stats.Changed, len(changedDirs))
	}
}

// TestQuickScanWorkerCountDoesNotAffectTheResult: the worker count is a
// performance knob, and a pass must reach the same index whatever it is set to.
func TestQuickScanWorkerCountDoesNotAffectTheResult(t *testing.T) {
	ctx := context.Background()
	for _, workers := range []int{1, 2, 8, 64} {
		f := newFixture(t)
		f.scanner.SetWorkers(workers)
		for i := range 20 {
			mustMkdirAll(t, filepath.Join(f.home, fmt.Sprintf("d%02d", i)))
			mustWrite(t, filepath.Join(f.home, fmt.Sprintf("d%02d", i), "a.txt"), "a")
		}
		f.scan(t)

		mustWrite(t, filepath.Join(f.home, "d07", "added.txt"), "added")
		if err := os.Remove(filepath.Join(f.home, "d13", "a.txt")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		before := f.node(t, "top.txt").ID
		if err := os.Rename(filepath.Join(f.home, "top.txt"),
			filepath.Join(f.home, "moved.txt")); err != nil {
			t.Fatalf("rename: %v", err)
		}

		if _, err := f.scanner.QuickScanUser(ctx, f.user); err != nil {
			t.Fatalf("workers=%d: QuickScanUser: %v", workers, err)
		}
		if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "d07/added.txt"); err != nil {
			t.Errorf("workers=%d: missed the added file: %v", workers, err)
		}
		if _, err := store.NodeByPath(ctx, f.db, f.user.ID, "d13/a.txt"); err == nil {
			t.Errorf("workers=%d: missed the deletion", workers)
		}
		after, err := store.NodeByPath(ctx, f.db, f.user.ID, "moved.txt")
		if err != nil {
			t.Errorf("workers=%d: missed the rename: %v", workers, err)
		} else if after.ID != before {
			t.Errorf("workers=%d: the rename lost the file's ID", workers)
		}
	}
}

// TestQuickScanStopsOnCancellation: a pass over a large share runs for a while,
// and shutdown must not wait for it. Workers have to notice too, not just the
// loop feeding them.
func TestQuickScanStopsOnCancellation(t *testing.T) {
	f := newFixture(t)
	for i := range 300 {
		mustMkdirAll(t, filepath.Join(f.home, fmt.Sprintf("d%03d", i)))
	}
	f.scan(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.scanner.QuickScanUser(ctx, f.user); !errors.Is(err, context.Canceled) {
		t.Errorf("QuickScanUser on a cancelled context = %v, want context.Canceled", err)
	}
}
