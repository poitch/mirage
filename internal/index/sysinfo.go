package index

import (
	"io/fs"

	"github.com/poitch/mirage/internal/fsx"
)

// devOf and inodeOf record filesystem identity alongside each entry. They are
// unused until rename detection lands, but capturing them during the scan that
// creates a node avoids a second pass over the whole tree later.
func devOf(fi fs.FileInfo) uint64 {
	dev, _, _ := fsx.Identity(fi)
	return dev
}

func inodeOf(fi fs.FileInfo) uint64 {
	_, inode, _ := fsx.Identity(fi)
	return inode
}
