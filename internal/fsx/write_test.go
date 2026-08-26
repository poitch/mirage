package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteFileAtomicity(t *testing.T) {
	s, _ := testStorage(t)

	res, err := s.WriteFile("docs/new.txt", strings.NewReader("brand new"), WriteOptions{})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if res.Size != int64(len("brand new")) {
		t.Errorf("Size = %d, want %d", res.Size, len("brand new"))
	}
	got, err := os.ReadFile(filepath.Join(s.Home(), "docs", "new.txt"))
	if err != nil || string(got) != "brand new" {
		t.Fatalf("content = %q, %v", got, err)
	}

	// A rejected write must leave the previous version exactly as it was.
	_, err = s.WriteFile("docs/new.txt", strings.NewReader("replacement"),
		WriteOptions{Checksum: "SHA1:" + strings.Repeat("0", 40)})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	got, _ = os.ReadFile(filepath.Join(s.Home(), "docs", "new.txt"))
	if string(got) != "brand new" {
		t.Errorf("a failed write damaged the existing file: %q", got)
	}
	assertNoTempFiles(t, filepath.Join(s.Home(), "docs"))
}

func TestWriteFileQuota(t *testing.T) {
	s, _ := testStorage(t)

	_, err := s.WriteFile("big.bin", strings.NewReader(strings.Repeat("x", 100)),
		WriteOptions{MaxBytes: 50})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	if _, err := os.Stat(filepath.Join(s.Home(), "big.bin")); !os.IsNotExist(err) {
		t.Error("an over-quota write left a file behind")
	}
	assertNoTempFiles(t, s.Home())

	// Exactly at the limit is allowed; the limit is a maximum, not a barrier.
	if _, err := s.WriteFile("exact.bin", strings.NewReader(strings.Repeat("x", 50)),
		WriteOptions{MaxBytes: 50}); err != nil {
		t.Errorf("a write of exactly MaxBytes was rejected: %v", err)
	}
}

func TestWriteFileChecksums(t *testing.T) {
	s, _ := testStorage(t)
	// sha1("hello") and md5("hello")
	const sha1Hello = "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"
	const md5Hello = "5d41402abc4b2a76b9719d911017c592"

	for _, spec := range []string{"SHA1:" + sha1Hello, "MD5:" + md5Hello, "sha1:" + strings.ToUpper(sha1Hello)} {
		if _, err := s.WriteFile("sum.txt", strings.NewReader("hello"),
			WriteOptions{Checksum: spec}); err != nil {
			t.Errorf("checksum %q rejected a matching body: %v", spec, err)
		}
	}

	// An algorithm we do not implement must be an error, not a silent pass:
	// skipping it would report verification that never happened.
	_, err := s.WriteFile("sum.txt", strings.NewReader("hello"),
		WriteOptions{Checksum: "SHA256:" + strings.Repeat("a", 64)})
	if err == nil {
		t.Error("an unsupported checksum algorithm was accepted")
	}
	if _, err := s.WriteFile("sum.txt", strings.NewReader("hello"),
		WriteOptions{Checksum: "garbage"}); err == nil {
		t.Error("a malformed checksum was accepted")
	}
}

func TestWriteFilePreservesMtime(t *testing.T) {
	s, _ := testStorage(t)
	want := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second)

	res, err := s.WriteFile("aged.txt", strings.NewReader("old"), WriteOptions{MTime: want})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !res.MTime.Truncate(time.Second).Equal(want) {
		t.Errorf("reported mtime = %v, want %v", res.MTime, want)
	}
	info, _ := os.Stat(filepath.Join(s.Home(), "aged.txt"))
	if !info.ModTime().Truncate(time.Second).Equal(want) {
		t.Errorf("mtime on disk = %v, want %v", info.ModTime(), want)
	}
}

// TestWriteFileRefusesEscape: the write path must be confined exactly as the
// read path is, and a mistake here would let one tenant plant files in another.
func TestWriteFileRefusesEscape(t *testing.T) {
	s, outside := testStorage(t)

	for _, name := range []string{
		"../bob/planted.txt",
		"docs/../../bob/planted.txt",
		"/etc/planted.txt",
		"",
		".",
	} {
		if _, err := s.WriteFile(name, strings.NewReader("x"), WriteOptions{}); err == nil {
			t.Errorf("WriteFile(%q) succeeded; it must not escape the home directory", name)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); !os.IsNotExist(err) {
		t.Fatal("a write escaped into the neighbouring directory")
	}

	if err := s.Mkdir("../bob/planted"); err == nil {
		t.Error("Mkdir escaped the home directory")
	}
	if err := s.Remove("../bob/secret.txt"); err == nil {
		t.Error("Remove escaped the home directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatalf("Remove deleted a file outside the home directory: %v", err)
	}
	if err := s.Remove("."); err == nil {
		t.Error("Remove deleted the home directory itself")
	}
}

func TestMkdirAndRemove(t *testing.T) {
	s, _ := testStorage(t)

	if err := s.Mkdir("fresh"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.Home(), "fresh"))
	if err != nil || !info.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}

	// Remove takes a whole tree, which is what DELETE on a collection means.
	if err := s.Remove("docs"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Home(), "docs")); !os.IsNotExist(err) {
		t.Error("directory still present after Remove")
	}
}

func TestCopyTree(t *testing.T) {
	s, _ := testStorage(t)

	if err := s.Copy("docs", "docs-copy"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.Home(), "docs-copy", "report.txt"))
	if err != nil || string(got) != "quarterly report" {
		t.Errorf("copied file = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(s.Home(), "docs", "report.txt")); err != nil {
		t.Errorf("Copy removed the source: %v", err)
	}

	// Copying a directory into itself would recurse without end.
	if err := s.Copy("docs", "docs/inner"); err == nil {
		t.Error("copying a directory into its own subtree was allowed")
	}
}

// TestOwnershipReporting covers goal 4's failure mode. Mirage must run as root
// to chown a file to another user; when it cannot, the file ends up owned by
// the server process and is unreachable over SMB, so that has to be reported
// rather than passing silently.
func TestOwnershipReporting(t *testing.T) {
	home, _ := testHome(t)

	t.Run("matching owner needs no chown", func(t *testing.T) {
		s, err := Open(home, os.Geteuid(), os.Getegid(), 0o640, 0o750)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()

		res, err := s.WriteFile("owned.txt", strings.NewReader("x"), WriteOptions{})
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if !res.OwnershipApplied {
			t.Error("ownership should be considered applied when it already matches")
		}
	})

	t.Run("foreign owner", func(t *testing.T) {
		const foreignUID, foreignGID = 12345, 12345
		s, err := Open(home, foreignUID, foreignGID, 0o640, 0o750)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()

		res, err := s.WriteFile("foreign.txt", strings.NewReader("x"), WriteOptions{})
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if os.Geteuid() != 0 {
			// Unprivileged: the write must still succeed, and must say the
			// ownership did not stick.
			if res.OwnershipApplied {
				t.Error("ownership reported as applied, but a non-root process cannot chown to another user")
			}
			return
		}
		// As root - which is how Mirage runs on the NAS - it must actually work.
		if !res.OwnershipApplied {
			t.Fatal("running as root, but ownership was not applied")
		}
		info, err := os.Stat(filepath.Join(home, "foreign.txt"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		uid, gid, ok := Owner(info)
		if !ok {
			t.Skip("ownership not reported on this filesystem")
		}
		if uid != foreignUID || gid != foreignGID {
			t.Errorf("file owned by %d:%d, want %d:%d", uid, gid, foreignUID, foreignGID)
		}
	})

	t.Run("directories are owned too", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("requires root to chown to another user")
		}
		const foreignUID, foreignGID = 12346, 12346
		s, err := Open(home, foreignUID, foreignGID, 0o640, 0o750)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()

		if err := s.Mkdir("owned-dir"); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		info, err := os.Stat(filepath.Join(home, "owned-dir"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		uid, gid, ok := Owner(info)
		if !ok {
			t.Skip("ownership not reported on this filesystem")
		}
		if uid != foreignUID || gid != foreignGID {
			t.Errorf("directory owned by %d:%d, want %d:%d", uid, gid, foreignUID, foreignGID)
		}
	})
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	for _, e := range entries {
		if IsInternal(e.Name()) {
			t.Errorf("a temporary file was left behind in %s: %s", dir, e.Name())
		}
	}
}
