package index

import (
	"testing"
	"time"
)

func TestFileETagChangesWithContent(t *testing.T) {
	base := time.Unix(1700000000, 0)
	original := FileETag(100, base)

	if FileETag(101, base) == original {
		t.Error("a change in size must change the ETag")
	}
	if FileETag(100, base.Add(time.Second)) == original {
		t.Error("a change in mtime must change the ETag")
	}
	// Nanosecond resolution matters: editors routinely rewrite a file more
	// than once within the same second.
	if FileETag(100, base.Add(time.Nanosecond)) == original {
		t.Error("a nanosecond change in mtime must change the ETag")
	}
	if FileETag(100, base) != original {
		t.Error("identical metadata must produce an identical ETag")
	}
	if len(original) != etagLen {
		t.Errorf("ETag length = %d, want %d", len(original), etagLen)
	}
}

// TestDirETagPropagatesChildChanges is the property the sync protocol depends
// on: a client skips a directory whose ETag it already knows, so any change
// below it has to surface here.
func TestDirETagPropagatesChildChanges(t *testing.T) {
	original := DirETag([]ChildDigest{{"a.txt", "e1"}, {"b.txt", "e2"}})

	if DirETag([]ChildDigest{{"a.txt", "CHANGED"}, {"b.txt", "e2"}}) == original {
		t.Error("a changed child ETag must change the directory ETag")
	}
	if DirETag([]ChildDigest{{"a.txt", "e1"}}) == original {
		t.Error("a removed child must change the directory ETag")
	}
	if DirETag([]ChildDigest{{"a.txt", "e1"}, {"b.txt", "e2"}, {"c.txt", "e3"}}) == original {
		t.Error("an added child must change the directory ETag")
	}
	if DirETag([]ChildDigest{{"renamed.txt", "e1"}, {"b.txt", "e2"}}) == original {
		t.Error("a renamed child must change the directory ETag")
	}
}

// TestDirETagIsStable keeps a rescan from looking like a change. If ordering or
// repetition shifted the value, every scan would trigger a full client resync.
func TestDirETagIsStable(t *testing.T) {
	a := DirETag([]ChildDigest{{"a.txt", "e1"}, {"b.txt", "e2"}, {"c.txt", "e3"}})
	b := DirETag([]ChildDigest{{"c.txt", "e3"}, {"a.txt", "e1"}, {"b.txt", "e2"}})
	if a != b {
		t.Error("directory ETag must not depend on the order children are listed in")
	}
	if DirETag(nil) != DirETag([]ChildDigest{}) {
		t.Error("nil and empty child lists must agree")
	}
}

// TestDirETagIsUnambiguous guards the length-prefixing: without it, adjacent
// fields could be re-split to produce the same hash from different trees.
func TestDirETagIsUnambiguous(t *testing.T) {
	a := DirETag([]ChildDigest{{"ab", "c"}})
	b := DirETag([]ChildDigest{{"a", "bc"}})
	if a == b {
		t.Error("different name/ETag splits must not hash alike")
	}
	c := DirETag([]ChildDigest{{"a", "b"}, {"c", "d"}})
	d := DirETag([]ChildDigest{{"ab", "cd"}})
	if c == d {
		t.Error("different child groupings must not hash alike")
	}
}
