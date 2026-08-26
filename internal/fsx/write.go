package fsx

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// ErrQuotaExceeded is returned when a write would take the user past their
// storage limit.
var ErrQuotaExceeded = errors.New("quota exceeded")

// ErrChecksumMismatch is returned when uploaded content does not match the
// checksum the client declared.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// WriteOptions controls a single file write.
type WriteOptions struct {
	// MTime, when non-zero, is applied to the finished file. Clients send it so
	// a file keeps its original timestamp across machines.
	MTime time.Time
	// MaxBytes stops the write once this many bytes have been read. Zero means
	// no limit. It is enforced during the copy rather than trusted from
	// Content-Length, which a client controls and may understate.
	MaxBytes int64
	// Checksum, in the form "SHA1:<hex>" or "MD5:<hex>", is verified before the
	// file is put into place.
	Checksum string
}

// WriteResult describes a completed write.
type WriteResult struct {
	Size  int64
	MTime time.Time
	// OwnershipApplied is false when the uid/gid could not be set, which means
	// the file is owned by whoever the server runs as. On a NAS that makes it
	// inaccessible over SMB, so callers surface it rather than ignoring it.
	OwnershipApplied bool
}

// WriteFile writes r to name, replacing any existing file atomically.
//
// The content goes to a temporary file in the same directory and is moved into
// place only once it is complete, verified, owned and stamped. Nothing ever
// observes a half-written file: not a sync client, and not somebody browsing
// the same share over SMB. A failed or aborted upload leaves the previous
// version untouched.
func (s *Storage) WriteFile(name string, r io.Reader, opts WriteOptions) (WriteResult, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return WriteResult{}, err
	}
	if IsRoot(clean) {
		return WriteResult{}, fmt.Errorf("%w: cannot write to the home directory itself", ErrInvalidPath)
	}

	digest, err := newDigest(opts.Checksum)
	if err != nil {
		return WriteResult{}, err
	}

	tmpPath, f, err := s.createTemp(path.Dir(clean))
	if err != nil {
		return WriteResult{}, err
	}
	// Any exit before the rename must not leave the temporary file behind.
	committed := false
	defer func() {
		if !committed {
			f.Close()              //nolint:errcheck // already failing
			s.root.Remove(tmpPath) //nolint:errcheck // best effort
		}
	}()

	var dst io.Writer = f
	if digest != nil {
		dst = io.MultiWriter(f, digest.hash)
	}

	src := r
	if opts.MaxBytes > 0 {
		// One byte past the limit, so exceeding it is detectable rather than
		// silently truncating the file.
		src = io.LimitReader(r, opts.MaxBytes+1)
	}
	size, err := io.Copy(dst, src)
	if err != nil {
		return WriteResult{}, fmt.Errorf("write %s: %w", name, err)
	}
	if opts.MaxBytes > 0 && size > opts.MaxBytes {
		return WriteResult{}, ErrQuotaExceeded
	}

	if digest != nil {
		if got := hex.EncodeToString(digest.hash.Sum(nil)); !strings.EqualFold(got, digest.want) {
			return WriteResult{}, fmt.Errorf("%w: declared %s, got %s", ErrChecksumMismatch, digest.want, got)
		}
	}

	// Durability before visibility: the rename must not be able to publish a
	// name whose contents have not reached disk.
	if err := f.Sync(); err != nil {
		return WriteResult{}, fmt.Errorf("sync %s: %w", name, err)
	}

	ownershipApplied, err := s.applyOwnership(f)
	if err != nil {
		return WriteResult{}, err
	}
	if err := f.Chmod(s.fileMode); err != nil && !errors.Is(err, os.ErrPermission) {
		return WriteResult{}, fmt.Errorf("set permissions on %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return WriteResult{}, fmt.Errorf("close %s: %w", name, err)
	}

	mtime := opts.MTime
	if !mtime.IsZero() {
		if err := s.root.Chtimes(tmpPath, time.Time{}, mtime); err != nil {
			return WriteResult{}, fmt.Errorf("set modification time on %s: %w", name, err)
		}
	}

	if err := s.root.Rename(tmpPath, clean); err != nil {
		return WriteResult{}, fmt.Errorf("publish %s: %w", name, err)
	}
	committed = true

	info, err := s.root.Stat(clean)
	if err != nil {
		return WriteResult{}, fmt.Errorf("stat %s after write: %w", name, err)
	}
	return WriteResult{Size: info.Size(), MTime: info.ModTime(), OwnershipApplied: ownershipApplied}, nil
}

// createTemp opens a uniquely named temporary file in dir.
//
// It has to be in the same directory as the destination: rename is only atomic
// within a filesystem, and on a NAS a different directory may well be a
// different volume.
func (s *Storage) createTemp(dir string) (string, *os.File, error) {
	for attempt := 0; attempt < 8; attempt++ {
		name := Join(dir, tempPrefix+randomSuffix())
		f, err := s.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, s.fileMode)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("create temporary file in %s: %w", dir, err)
		}
	}
	return "", nil, fmt.Errorf("create temporary file in %s: too many collisions", dir)
}

func randomSuffix() string {
	var b [8]byte
	//nolint:errcheck // crypto/rand.Read does not fail
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// applyOwnership stamps the user's uid and gid onto an open file.
//
// It uses the descriptor rather than the path, which makes it fchown: there is
// no window in which the name could be swapped for a symlink between the check
// and the change.
//
// A permission failure is reported rather than fatal. Mirage must run as root
// to chown to another user, and when it does not - a development machine, or a
// misconfigured container - refusing every upload would be worse than writing
// the file under the wrong owner and saying so.
func (s *Storage) applyOwnership(f *os.File) (bool, error) {
	if s.uid == os.Geteuid() && s.gid == os.Getegid() {
		// Already correct; nothing to do and nothing to warn about.
		return true, nil
	}
	err := f.Chown(s.uid, s.gid)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrInvalid) {
		return false, nil
	}
	return false, fmt.Errorf("set ownership: %w", err)
}

// Mkdir creates a directory, applying the configured ownership and permissions.
func (s *Storage) Mkdir(name string) error {
	clean, err := CleanPath(name)
	if err != nil {
		return err
	}
	if IsRoot(clean) {
		return fmt.Errorf("%w: the home directory already exists", ErrInvalidPath)
	}
	if err := s.root.Mkdir(clean, s.dirMode); err != nil {
		return err
	}

	// Mkdir gives no descriptor, so the directory is reopened to chown it
	// through a handle rather than by name.
	d, err := s.root.Open(clean)
	if err != nil {
		return fmt.Errorf("reopen %s to set ownership: %w", name, err)
	}
	defer d.Close()
	if _, err := s.applyOwnership(d); err != nil {
		return err
	}
	if err := d.Chmod(s.dirMode); err != nil && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("set permissions on %s: %w", name, err)
	}
	return nil
}

// Remove deletes a file, or a directory and everything beneath it.
func (s *Storage) Remove(name string) error {
	clean, err := CleanPath(name)
	if err != nil {
		return err
	}
	if IsRoot(clean) {
		return fmt.Errorf("%w: cannot delete the home directory", ErrInvalidPath)
	}
	return s.root.RemoveAll(clean)
}

// Rename moves a file or directory within the user's home.
func (s *Storage) Rename(oldName, newName string) error {
	oldClean, err := CleanPath(oldName)
	if err != nil {
		return err
	}
	newClean, err := CleanPath(newName)
	if err != nil {
		return err
	}
	if IsRoot(oldClean) || IsRoot(newClean) {
		return fmt.Errorf("%w: cannot move the home directory", ErrInvalidPath)
	}
	return s.root.Rename(oldClean, newClean)
}

// Copy duplicates a file or a directory tree.
func (s *Storage) Copy(srcName, dstName string) error {
	srcClean, err := CleanPath(srcName)
	if err != nil {
		return err
	}
	dstClean, err := CleanPath(dstName)
	if err != nil {
		return err
	}
	if IsRoot(dstClean) {
		return fmt.Errorf("%w: cannot copy over the home directory", ErrInvalidPath)
	}
	// Copying a directory into its own subtree would recurse without end.
	if srcClean != dstClean && strings.HasPrefix(dstClean, srcClean+"/") {
		return fmt.Errorf("%w: cannot copy a directory into itself", ErrInvalidPath)
	}
	info, err := s.root.Stat(srcClean)
	if err != nil {
		return err
	}
	return s.copyTree(srcClean, dstClean, info)
}

func (s *Storage) copyTree(src, dst string, info fs.FileInfo) error {
	if !info.IsDir() {
		f, err := s.root.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = s.WriteFile(dst, f, WriteOptions{MTime: info.ModTime()})
		return err
	}

	if err := s.Mkdir(dst); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	entries, err := s.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		childInfo, err := e.Info()
		if err != nil {
			continue // vanished mid-copy; nothing useful to copy
		}
		if err := s.copyTree(Join(src, e.Name()), Join(dst, e.Name()), childInfo); err != nil {
			return err
		}
	}
	return nil
}

// digest tracks a declared checksum while content streams past.
type digest struct {
	hash hash.Hash
	want string
}

// newDigest parses an OC-Checksum value such as "SHA1:2ef7bde...". An empty
// value means the client declared nothing to verify.
func newDigest(spec string) (*digest, error) {
	if spec == "" {
		return nil, nil
	}
	algo, want, found := strings.Cut(spec, ":")
	if !found {
		return nil, fmt.Errorf("malformed checksum %q: expected ALGO:HEX", spec)
	}
	switch strings.ToUpper(strings.TrimSpace(algo)) {
	case "SHA1":
		return &digest{hash: sha1.New(), want: want}, nil
	case "MD5":
		return &digest{hash: md5.New(), want: want}, nil
	default:
		// Only the algorithms advertised in capabilities are accepted; silently
		// skipping an unknown one would report success without checking.
		return nil, fmt.Errorf("unsupported checksum algorithm %q", algo)
	}
}
