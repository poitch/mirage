package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Cache keeps generated previews on disk.
//
// They live in Mirage's own data directory rather than in the account's files.
// Putting them in a home would sync a folder of thumbnails to every device,
// count them against the person's quota, and show them over SMB. Here they are
// nobody's business but the server's, and losing the lot costs only the work of
// making them again.
type Cache struct {
	dir string
	// sem bounds how many previews are made at once. Each one reads a whole
	// photograph off the disk, and a gallery scrolling on a phone will ask for
	// dozens at a time; without this they would all seek at once and the array
	// would spend the next minute doing nothing else.
	sem chan struct{}
}

// NewCache opens the cache directory, creating it if needed.
func NewCache(dir string, concurrency int) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("preview cache directory must be set")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create preview cache: %w", err)
	}
	return &Cache{dir: dir, sem: make(chan struct{}, max(concurrency, 1))}, nil
}

// Key identifies a preview.
//
// The file's ETag is part of it, so editing a photograph produces a different
// key and the old preview is simply never asked for again rather than having to
// be found and deleted.
func Key(userID, fileID int64, etag string, size int) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%d|%s|%d|1", userID, fileID, etag, size)
	return hex.EncodeToString(h.Sum(nil))
}

// path spreads entries over 256 directories, because a single directory with a
// million files in it is slow to open on every filesystem worth naming.
func (c *Cache) path(key string) string {
	return filepath.Join(c.dir, key[:2], key+".jpg")
}

// Get returns a cached preview, or nil if there is none.
func (c *Cache) Get(key string) []byte {
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil
	}
	// Touched so that pruning can tell a preview somebody looks at from one
	// made once and forgotten.
	now := time.Now()
	_ = os.Chtimes(c.path(key), now, now)
	return data
}

// Put stores a preview. A failure to write is not reported: the preview was
// produced and can be sent, and the only cost is making it again next time.
func (c *Cache) Put(key string, data []byte) {
	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	// Written beside and renamed, so a reader never sees half a file.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Chmod(tmp.Name(), 0o600)
	_ = os.Rename(tmp.Name(), p)
}

// Acquire takes a generation slot, waiting until one is free or the caller
// gives up. The returned function releases it.
func (c *Cache) Acquire(done <-chan struct{}) (release func(), ok bool) {
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, true
	case <-done:
		return func() {}, false
	}
}

// Prune deletes previews nothing has asked for in a while.
//
// Bounded by age rather than by size, because age is what a stale entry has:
// once a photograph is edited or deleted its previews are unreachable by key
// and would otherwise sit there for the life of the installation.
func (c *Cache) Prune(maxAge time.Duration) (removed int, freed int64, err error) {
	if maxAge <= 0 {
		return 0, 0, nil
	}
	cutoff := time.Now().Add(-maxAge)

	err = filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not worth failing the sweep
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil //nolint:nilerr // as above
		}
		size := info.Size()
		if os.Remove(p) == nil {
			removed++
			freed += size
		}
		return nil
	})
	return removed, freed, err
}

// entryCount is used by tests and by the status command.
func (c *Cache) entryCount() (int, error) {
	n := 0
	err := filepath.WalkDir(c.dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n, err
}
