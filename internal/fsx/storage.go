package fsx

import (
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// Storage gives access to one user's files, confined to their home directory.
//
// Confinement is enforced by os.Root: every operation resolves through a
// directory handle, and the kernel refuses any resolution that would leave it,
// including one that goes through a symlink. Tenant isolation therefore does
// not depend on Mirage getting string handling right.
type Storage struct {
	root     *os.Root
	home     string
	uid, gid int
	fileMode fs.FileMode
	dirMode  fs.FileMode
}

// Open opens a user's home directory for confined access.
func Open(home string, uid, gid int, fileMode, dirMode fs.FileMode) (*Storage, error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, fmt.Errorf("open home %s: %w", home, err)
	}
	return &Storage{
		root: root, home: home,
		uid: uid, gid: gid,
		fileMode: fileMode.Perm(), dirMode: dirMode.Perm(),
	}, nil
}

// Home returns the host path of the user's home directory.
func (s *Storage) Home() string { return s.home }

// Owner returns the uid and gid stamped on files this user owns.
func (s *Storage) Owner() (uid, gid int) { return s.uid, s.gid }

// Close releases the directory handle.
func (s *Storage) Close() error { return s.root.Close() }

// Stat returns file information for a path relative to the user's home.
func (s *Storage) Stat(name string) (fs.FileInfo, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return nil, err
	}
	return s.root.Stat(clean)
}

// Open opens a file for reading.
func (s *Storage) Open(name string) (*os.File, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return nil, err
	}
	return s.root.Open(clean)
}

// ReadDir lists a directory, omitting Mirage's internal entries.
//
// Entries whose type cannot be determined are dropped rather than guessed at:
// a broken symlink or a file removed mid-listing is not an error worth failing
// the whole request over, and including it would put a phantom into the index.
func (s *Storage) ReadDir(name string) ([]fs.DirEntry, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(clean)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}

	out := entries[:0]
	for _, e := range entries {
		if IsInternal(e.Name()) {
			continue
		}
		// Only regular files and directories are represented. Info reports on
		// the entry itself rather than a symlink's target, so symlinks are
		// filtered out here along with sockets, devices and FIFOs: none of them
		// have meaning to a sync client, and following a link risks both cycles
		// and a scan that blocks indefinitely on a special file.
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Manager holds one Storage per user, opened on demand and reused.
//
// Keeping the handle open is what makes confinement cheap: the directory is
// resolved once, and every later operation is relative to that descriptor.
type Manager struct {
	mu       sync.RWMutex
	byUser   map[int64]*Storage
	fileMode fs.FileMode
	dirMode  fs.FileMode
	closed   bool
}

// NewManager builds a Manager applying the given permissions to new files.
func NewManager(fileMode, dirMode fs.FileMode) *Manager {
	return &Manager{
		byUser:   make(map[int64]*Storage),
		fileMode: fileMode.Perm(),
		dirMode:  dirMode.Perm(),
	}
}

// ErrClosed is returned once the Manager has been shut down.
var ErrClosed = fmt.Errorf("storage manager closed")

// For returns the Storage for a user, opening it if this is the first use.
func (m *Manager) For(userID int64, home string, uid, gid int) (*Storage, error) {
	m.mu.RLock()
	if s, ok := m.byUser[userID]; ok {
		m.mu.RUnlock()
		return s, nil
	}
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Another goroutine may have opened it while the lock was released.
	if s, ok := m.byUser[userID]; ok {
		return s, nil
	}
	if m.closed {
		return nil, ErrClosed
	}
	s, err := Open(home, uid, gid, m.fileMode, m.dirMode)
	if err != nil {
		return nil, err
	}
	m.byUser[userID] = s
	return s, nil
}

// Forget closes and drops a user's Storage, so the next use reopens it. It is
// called when a user's home directory changes underneath us.
func (m *Manager) Forget(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.byUser[userID]; ok {
		s.Close() //nolint:errcheck // dropping it regardless
		delete(m.byUser, userID)
	}
}

// Close releases every open handle.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	var firstErr error
	for id, s := range m.byUser {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.byUser, id)
	}
	return firstErr
}
