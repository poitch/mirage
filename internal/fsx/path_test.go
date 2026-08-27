package fsx

import (
	"errors"
	"testing"
)

func TestCleanPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", RootPath},
		{"/", RootPath},
		{".", RootPath},
		{"docs", "docs"},
		{"/docs/", "docs"},
		{"docs/report.txt", "docs/report.txt"},
		{"/docs//report.txt", "docs/report.txt"},
		{"docs/./report.txt", "docs/report.txt"},
		{"a/b/c/d.txt", "a/b/c/d.txt"},
		// Names that merely look suspicious are ordinary filenames.
		{"..hidden", "..hidden"},
		{"file..txt", "file..txt"},
		{"...", "..."},
	}
	for _, tc := range tests {
		got, err := CleanPath(tc.in)
		if err != nil {
			t.Errorf("CleanPath(%q) returned error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CleanPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanPathRejectsTraversal(t *testing.T) {
	// Each of these must be refused outright. "a/../../b" is the one that
	// matters most: resolving ".." against the accumulated path would cancel
	// the "a" segment and let the rest climb out of the home directory.
	for _, in := range []string{
		"..",
		"../",
		"../etc/passwd",
		"docs/../..",
		"a/../../b",
		"docs/../../../../etc/shadow",
		"/../secret",
		"docs\\..\\..\\secret",
		"docs/\x00/passwd",
		"\x00",
	} {
		if got, err := CleanPath(in); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanPath(%q) = %q, err = %v; want ErrInvalidPath", in, got, err)
		}
	}
}

func TestIsInternal(t *testing.T) {
	if !IsInternal(".mirage-tmp-abc123") {
		t.Error("partial upload should be treated as internal")
	}
	// Synology puts @eaDir in every directory on the volume. Indexing it would
	// push thumbnails and indexer metadata to every device.
	for _, name := range []string{"@eaDir", "#recycle", "#snapshot", "@tmp", ".mirage-uploads"} {
		if !IsInternal(name) {
			t.Errorf("IsInternal(%q) = false; filesystem machinery must not be indexed", name)
		}
	}
	// Real files that merely resemble them must still sync.
	for _, name := range []string{
		"report.txt", ".hidden", ".mirage", "mirage-tmp-x",
		"@eaDir.txt", "recycle", "#recycled", "my@eaDir",
	} {
		if IsInternal(name) {
			t.Errorf("IsInternal(%q) = true, want false", name)
		}
	}
}

func TestJoin(t *testing.T) {
	if got := Join(RootPath, "docs"); got != "docs" {
		t.Errorf("Join(root, docs) = %q, want docs", got)
	}
	if got := Join("docs", "a.txt"); got != "docs/a.txt" {
		t.Errorf("Join(docs, a.txt) = %q, want docs/a.txt", got)
	}
}
