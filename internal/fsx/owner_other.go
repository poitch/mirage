//go:build !unix

package fsx

import "io/fs"

// Owner reports no ownership on non-Unix platforms. Mirage's target is a Linux
// container; this exists so the tree still builds elsewhere for development.
func Owner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

// Identity reports no device/inode identity on non-Unix platforms.
func Identity(fs.FileInfo) (dev, inode uint64, ok bool) { return 0, 0, false }
