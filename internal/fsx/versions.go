package fsx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Keeping an earlier copy of a file means copying it, not linking to it.
//
// A hard link would be instant and free, and would be wrong: it shares one
// inode with the live file, so anything that writes in place - which is what
// SMB clients and most editors do - would rewrite the history along with the
// file. A copy is independent, which is the entire point of a version.
//
// The cost is real, and is why versions are only kept for files below a size
// the operator sets.

// VersionPath is where one version of a file is stored.
func VersionPath(fileID int64, timestamp int64) string {
	return VersionsDir + "/" + strconv.FormatInt(fileID, 10) + "/" + strconv.FormatInt(timestamp, 10)
}

// SaveVersion copies the current contents of a file aside.
//
// Copied before the new contents are written, and reported as an error if it
// fails, so that a save cannot quietly lose the only copy of what was there.
func (s *Storage) SaveVersion(name string, fileID, timestamp int64) (int64, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return 0, err
	}
	dir := VersionsDir + "/" + strconv.FormatInt(fileID, 10)
	if err := s.MkdirAll(dir); err != nil {
		return 0, fmt.Errorf("create version directory: %w", err)
	}

	src, err := s.root.Open(clean)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return 0, errors.New("cannot version a directory")
	}

	target := VersionPath(fileID, timestamp)
	// Written to a temporary name and renamed, so a version is never half
	// written if the server stops in the middle.
	tmpName, tmp, err := s.createTemp(dir)
	if err != nil {
		return 0, err
	}
	defer func() {
		tmp.Close()
		s.root.Remove(tmpName) //nolint:errcheck // best effort; already renamed on success
	}()

	written, err := io.Copy(tmp, src)
	if err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if _, err := s.applyOwnership(tmp); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := s.root.Rename(tmpName, target); err != nil {
		return 0, err
	}
	// The version keeps the file's own timestamp, so a listing shows when the
	// contents were current rather than when the copy happened.
	if err := s.root.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
		return written, nil //nolint:nilerr // cosmetic; the version itself is intact
	}
	return written, nil
}

// OpenVersion opens a stored version for reading.
func (s *Storage) OpenVersion(fileID, timestamp int64) (*os.File, error) {
	return s.root.Open(VersionPath(fileID, timestamp))
}

// StatVersion reports on a stored version.
func (s *Storage) StatVersion(fileID, timestamp int64) (fs.FileInfo, error) {
	return s.root.Stat(VersionPath(fileID, timestamp))
}

// RestoreVersion writes a stored version back over a path.
//
// The live file is not saved as a version here; the caller does that first, so
// that restoring is itself undoable.
func (s *Storage) RestoreVersion(fileID, timestamp int64, dest string, opts WriteOptions) (WriteResult, error) {
	f, err := s.OpenVersion(fileID, timestamp)
	if err != nil {
		return WriteResult{}, err
	}
	defer f.Close()
	return s.WriteFile(dest, f, opts)
}

// RemoveVersion deletes one stored version.
func (s *Storage) RemoveVersion(fileID, timestamp int64) error {
	return s.Remove(VersionPath(fileID, timestamp))
}

// RemoveVersionsFor deletes every version of a file, for when the file itself
// is gone for good.
func (s *Storage) RemoveVersionsFor(fileID int64) error {
	dir := VersionsDir + "/" + strconv.FormatInt(fileID, 10)
	err := s.Remove(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// VersionedFileIDs lists the files that have versions stored, for reconciling
// the directory against the index.
func (s *Storage) VersionedFileIDs() ([]int64, error) {
	d, err := s.root.Open(VersionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()

	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		if id, err := strconv.ParseInt(e.Name(), 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}
