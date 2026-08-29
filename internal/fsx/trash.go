package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Deleting moves a file into a hidden directory inside the account rather than
// unlinking it, so that somebody who deletes the wrong thing can get it back.
//
// It is a rename, which is why the trash has to live inside the account's own
// directory: renaming across filesystems is not possible, and copying five
// gigabytes to delete it would be absurd. The cost is that trashed files still
// occupy the person's disk until they expire.

// ErrTrashOccupied is returned when restoring would overwrite something.
var ErrTrashOccupied = errors.New("something is already at that path")

// TrashName builds the name a deleted file is stored under.
//
// The deletion time is part of it so that deleting two files of the same name
// from different folders does not collide, and so that expiry can be decided
// from the name alone if the index is ever rebuilt.
func TrashName(original string, deletedAt time.Time) string {
	return path.Base(original) + ".d" + fmt.Sprint(deletedAt.Unix())
}

// MoveToTrash moves a path into the account's trash under trashName.
func (s *Storage) MoveToTrash(name, trashName string) error {
	clean, err := CleanPath(name)
	if err != nil {
		return err
	}
	if IsRoot(clean) {
		return errors.New("cannot delete the account root")
	}
	if err := s.ensureTrashDir(); err != nil {
		return err
	}
	// A collision here would silently destroy the earlier deletion, so it is
	// checked rather than left to Rename, which would overwrite.
	target := TrashDir + "/" + trashName
	if _, err := s.root.Stat(target); err == nil {
		return fmt.Errorf("trash entry %s: %w", trashName, ErrTrashOccupied)
	}
	return s.root.Rename(clean, target)
}

// RestoreFromTrash moves a trashed entry back to a path in the account.
//
// The parent is recreated if it has gone, which is common: deleting a folder
// and then restoring one file out of it has nowhere to put it otherwise.
func (s *Storage) RestoreFromTrash(trashName, dest string) error {
	clean, err := CleanPath(dest)
	if err != nil {
		return err
	}
	if IsRoot(clean) {
		return errors.New("cannot restore over the account root")
	}
	source := TrashDir + "/" + trashName
	if _, err := s.root.Stat(source); err != nil {
		return err
	}
	// Refused rather than overwritten: restoring must not be a way to lose a
	// file that is currently there. The caller picks a free name instead.
	if _, err := s.root.Stat(clean); err == nil {
		return fmt.Errorf("restore to %s: %w", clean, ErrTrashOccupied)
	}
	if parent := path.Dir(clean); parent != "." && parent != "/" {
		if err := s.MkdirAll(parent); err != nil {
			return err
		}
	}
	return s.root.Rename(source, clean)
}

// RemoveFromTrash deletes a trashed entry for good.
func (s *Storage) RemoveFromTrash(trashName string) error {
	if trashName == "" || strings.ContainsAny(trashName, `/\`) {
		return fmt.Errorf("invalid trash entry %q", trashName)
	}
	return s.Remove(TrashDir + "/" + trashName)
}

// TrashEntries lists what is physically in the trash directory.
//
// Used to reconcile the index against the disk, since the two can drift if
// somebody empties the directory over SMB.
func (s *Storage) TrashEntries() ([]fs.DirEntry, error) {
	entries, err := s.root.Open(TrashDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer entries.Close()
	return entries.ReadDir(-1)
}

// ensureTrashDir creates the trash directory, owned like everything else Mirage
// makes so that the person can still manage it over SMB.
func (s *Storage) ensureTrashDir() error {
	if _, err := s.root.Stat(TrashDir); err == nil {
		return nil
	}
	return s.Mkdir(TrashDir)
}

// MkdirAll creates a directory and any parents it needs, inside the account.
func (s *Storage) MkdirAll(name string) error {
	clean, err := CleanPath(name)
	if err != nil {
		return err
	}
	if IsRoot(clean) {
		return nil
	}
	if info, err := s.root.Stat(clean); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", clean)
		}
		return nil
	}
	if parent := path.Dir(clean); parent != "." && parent != "/" {
		if err := s.MkdirAll(parent); err != nil {
			return err
		}
	}
	if err := s.Mkdir(clean); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
