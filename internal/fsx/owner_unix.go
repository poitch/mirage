//go:build unix

package fsx

import (
	"io/fs"
	"syscall"
)

// Owner returns the uid and gid recorded in fi. ok is false on platforms or
// filesystems that do not report Unix ownership.
func Owner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// Identity returns the device and inode numbers for fi. Together they identify
// a file across renames, which is how an out-of-band move is told apart from a
// delete-plus-create during a rescan. ok is false where unavailable.
func Identity(fi fs.FileInfo) (dev, inode uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
