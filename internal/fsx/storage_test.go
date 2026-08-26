package fsx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testHome builds a user home with a few files, plus a sibling directory that
// stands in for "another tenant's data" and must stay unreachable.
func testHome(t *testing.T) (home, outside string) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "alice")
	outside = filepath.Join(base, "bob")

	for _, dir := range []string{home, outside, filepath.Join(home, "docs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(home, "hello.txt"), "hello from alice")
	write(filepath.Join(home, "docs", "report.txt"), "quarterly report")
	write(filepath.Join(outside, "secret.txt"), "bob's private data")
	return home, outside
}

func testStorage(t *testing.T) (*Storage, string) {
	t.Helper()
	home, outside := testHome(t)
	s, err := Open(home, os.Getuid(), os.Getgid(), 0o640, 0o750)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, outside
}

func TestStorageReadsOwnFiles(t *testing.T) {
	s, _ := testStorage(t)

	f, err := s.Open("docs/report.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "quarterly report" {
		t.Errorf("content = %q, want %q", got, "quarterly report")
	}

	fi, err := s.Stat("hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != int64(len("hello from alice")) {
		t.Errorf("size = %d, want %d", fi.Size(), len("hello from alice"))
	}
}

// TestStorageRefusesTraversal is the tenant-isolation guarantee stated as a
// test: no textual path may reach outside the home directory.
func TestStorageRefusesTraversal(t *testing.T) {
	s, _ := testStorage(t)

	for _, name := range []string{
		"../bob/secret.txt",
		"docs/../../bob/secret.txt",
		"../../etc/passwd",
		"/etc/passwd",
		"docs/../../../../../../etc/passwd",
	} {
		if _, err := s.Open(name); err == nil {
			t.Errorf("Open(%q) succeeded; it must not escape the home directory", name)
		}
		if _, err := s.Stat(name); err == nil {
			t.Errorf("Stat(%q) succeeded; it must not escape the home directory", name)
		}
	}
}

// TestStorageRefusesSymlinkEscape covers the case string validation cannot
// catch: the path is entirely well-formed, and the escape happens in the
// kernel's resolution of a symlink the user planted. os.Root is what stops it.
func TestStorageRefusesSymlinkEscape(t *testing.T) {
	s, outside := testStorage(t)
	home := s.Home()

	links := map[string]string{
		"escape-abs":  outside,                              // absolute, to another tenant
		"escape-rel":  filepath.Join("..", "bob"),           // relative climb
		"escape-root": "/etc",                               // absolute, outside everything
		"escape-file": filepath.Join(outside, "secret.txt"), // straight at the file
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(home, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}

	for name := range links {
		if _, err := s.Open(name); err == nil {
			t.Errorf("Open(%q) followed a symlink out of the home directory", name)
		}
		if _, err := s.Open(name + "/secret.txt"); err == nil {
			t.Errorf("Open(%q/secret.txt) followed a symlink out of the home directory", name)
		}
	}

	// The other tenant's file is still there; it is the access that is denied,
	// not the fixture that is missing.
	if _, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatalf("fixture check: %v", err)
	}
}

// TestStorageSurvivesSymlinkedHome: a Synology share is very often reached
// through a symlink, so the home itself being one must be fine. Only escaping
// *from* the home is forbidden.
func TestStorageSurvivesSymlinkedHome(t *testing.T) {
	home, _ := testHome(t)
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	s, err := Open(link, os.Getuid(), os.Getgid(), 0o640, 0o750)
	if err != nil {
		t.Fatalf("Open through symlinked home: %v", err)
	}
	defer s.Close()

	if _, err := s.Stat("hello.txt"); err != nil {
		t.Errorf("Stat through symlinked home: %v", err)
	}
}

func TestReadDirHidesInternalAndSpecialEntries(t *testing.T) {
	s, _ := testStorage(t)
	home := s.Home()

	if err := os.WriteFile(filepath.Join(home, ".mirage-tmp-abc"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	// A dangling symlink has no resolvable type and must simply be skipped
	// rather than failing the whole listing.
	if err := os.Symlink("/nonexistent/target", filepath.Join(home, "dangling")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	entries, err := s.ReadDir(RootPath)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	joined := strings.Join(names, ",")

	for _, want := range []string{"hello.txt", "docs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ReadDir omitted %q; got %v", want, names)
		}
	}
	for _, unwanted := range []string{".mirage-tmp-abc", "dangling"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("ReadDir exposed %q; got %v", unwanted, names)
		}
	}
}

func TestManagerReusesAndForgets(t *testing.T) {
	home, _ := testHome(t)
	m := NewManager(0o640, 0o750)
	defer m.Close()

	a, err := m.For(1, home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	b, err := m.For(1, home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("For (second): %v", err)
	}
	if a != b {
		t.Error("Manager opened a second handle for the same user")
	}

	m.Forget(1)
	c, err := m.For(1, home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("For after Forget: %v", err)
	}
	if c == a {
		t.Error("Forget did not drop the cached handle")
	}
}

func TestManagerRejectsMissingHome(t *testing.T) {
	m := NewManager(0o640, 0o750)
	defer m.Close()

	if _, err := m.For(1, filepath.Join(t.TempDir(), "nope"), 0, 0); err == nil {
		t.Fatal("For succeeded for a home directory that does not exist")
	}
}

func TestManagerAfterClose(t *testing.T) {
	home, _ := testHome(t)
	m := NewManager(0o640, 0o750)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.For(1, home, 0, 0); !errors.Is(err, ErrClosed) {
		t.Errorf("For after Close: err = %v, want ErrClosed", err)
	}
}
