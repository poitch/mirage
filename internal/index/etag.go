// Package index maintains Mirage's view of the filesystem: the file IDs, sizes
// and ETags that sync clients rely on.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"time"
)

// etagLen is how many hex characters an ETag carries. Sixteen bytes makes an
// accidental collision irrelevant while keeping PROPFIND responses compact.
const etagLen = 32

// FileETag derives a file's ETag from its size and modification time.
//
// Hashing the content instead would be exact, but it would mean reading every
// byte of every file on every scan, which is unaffordable on a NAS. This is the
// same trade Nextcloud makes, and it carries the same edge: a file rewritten
// with identical length and an identical nanosecond timestamp is not noticed.
// In practice that requires deliberately restoring the timestamp.
func FileETag(size int64, mtime time.Time) string {
	h := sha256.New()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(size))
	h.Write(buf[:])
	binary.LittleEndian.PutUint64(buf[:], uint64(mtime.UnixNano()))
	h.Write(buf[:])
	return hex.EncodeToString(h.Sum(nil))[:etagLen]
}

// ChildDigest is one entry's contribution to its parent's ETag.
type ChildDigest struct {
	Name string
	ETag string
}

// DirETag derives a directory's ETag from the names and ETags of its children.
//
// This is the mechanism the whole sync protocol rests on. A client walks the
// tree comparing directory ETags and does not descend into one that has not
// changed, so a change anywhere must alter every ETag above it. Deriving a
// directory's ETag from its children makes that propagation automatic.
//
// It is also deterministic, which matters more than it first appears: a full
// rescan recomputes the same value for an unchanged tree, so rescanning does
// not itself look like a change and does not trigger clients to resynchronise.
func DirETag(children []ChildDigest) string {
	sorted := make([]ChildDigest, len(children))
	copy(sorted, children)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	for _, c := range sorted {
		// Length-prefixed so that ("ab", "c") and ("a", "bc") cannot collide.
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(c.Name)))
		h.Write(buf[:])
		h.Write([]byte(c.Name))
		binary.LittleEndian.PutUint64(buf[:], uint64(len(c.ETag)))
		h.Write(buf[:])
		h.Write([]byte(c.ETag))
	}
	return hex.EncodeToString(h.Sum(nil))[:etagLen]
}
