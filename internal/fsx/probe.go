package fsx

import (
	"fmt"
	"os"
	"path/filepath"
)

// Probe describes the state of a mapped home directory.
//
// Getting this mapping wrong is the most likely first-run problem on a NAS, and
// it otherwise surfaces as opaque permission errors partway through a sync. The
// same probe backs `mirage doctor` and the admin page, so both say the same
// thing about the same directory.
type Probe struct {
	Path string

	Exists   bool
	IsDir    bool
	Writable bool

	// OwnerKnown is false on filesystems that do not report Unix ownership.
	OwnerKnown bool
	OwnerUID   int
	OwnerGID   int
	Mode       os.FileMode

	// WantUID and WantGID are the values Mirage was told to stamp on new files.
	WantUID int
	WantGID int

	// Problem is a human-readable description of what is wrong, or "" when the
	// directory is usable.
	Problem string
	// Warning is set when the directory works but something looks off.
	Warning string
}

// OK reports whether the directory is usable.
func (p Probe) OK() bool { return p.Problem == "" }

// OwnershipMatches reports whether the directory's own ownership agrees with
// what Mirage will stamp on the files it creates inside it.
func (p Probe) OwnershipMatches() bool {
	return p.OwnerKnown && p.OwnerUID == p.WantUID && p.OwnerGID == p.WantGID
}

// ProbeHome inspects a mapped home directory.
func ProbeHome(path string, wantUID, wantGID int) Probe {
	p := Probe{Path: path, WantUID: wantUID, WantGID: wantGID}

	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		p.Problem = fmt.Sprintf("does not exist inside the container; check the bind mount for %s",
			filepath.Dir(path))
		return p
	case err != nil:
		p.Problem = fmt.Sprintf("cannot be read: %v", err)
		return p
	}
	p.Exists = true
	p.IsDir = info.IsDir()
	p.Mode = info.Mode().Perm()
	if !p.IsDir {
		p.Problem = "exists but is not a directory"
		return p
	}

	if uid, gid, ok := Owner(info); ok {
		p.OwnerKnown, p.OwnerUID, p.OwnerGID = true, uid, gid
	}

	if err := checkWritable(path); err != nil {
		p.Problem = fmt.Sprintf("is not writable: %v", err)
		return p
	}
	p.Writable = true

	// Not fatal: Mirage stamps its configured uid and gid on what it writes
	// regardless. But a mismatch usually means the values were copied from
	// another account, and new files then will not match their own directory.
	if p.OwnerKnown && !p.OwnershipMatches() {
		p.Warning = fmt.Sprintf("the directory is owned by %d:%d, but files will be created as %d:%d",
			p.OwnerUID, p.OwnerGID, wantUID, wantGID)
	}
	if os.Geteuid() != 0 && (wantUID != os.Geteuid() || wantGID != os.Getegid()) {
		p.Warning = "Mirage is not running as root, so it cannot apply this ownership; " +
			"files will belong to the server process and be unreachable over SMB"
	}
	return p
}

// checkWritable confirms the directory accepts a create-and-remove cycle.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, tempPrefix+"probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close() //nolint:errcheck // being removed next
	return os.Remove(name)
}
