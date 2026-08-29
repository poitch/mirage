// Package fsx provides per-user filesystem access confined to that user's home
// directory.
package fsx

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrInvalidPath is returned for a path that is malformed or tries to escape
// the user's home directory.
var ErrInvalidPath = errors.New("invalid path")

// RootPath is how the root of a user's home is named to os.Root.
const RootPath = "."

// CleanPath normalises a slash-separated path relative to a user's home and
// returns it in the form os.Root expects.
//
// This is a first line of defence, not the only one: os.Root refuses to resolve
// outside its directory even for inputs that get past here. Rejecting the
// obvious cases early gives a clear error instead of a confusing syscall
// failure, and keeps traversal attempts out of the index.
func CleanPath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", ErrInvalidPath
	}
	// A leading slash is how WebDAV writes a path relative to the collection
	// root, so it is stripped rather than treated as absolute.
	name = strings.Trim(name, "/")
	if name == "" || name == "." {
		return RootPath, nil
	}

	parts := strings.Split(name, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "", ".":
			// Collapse empty and current-directory segments.
			continue
		case "..":
			// Not resolved against the accumulated path: allowing that would
			// let "a/../../b" escape by cancelling a segment we never entered.
			return "", ErrInvalidPath
		}
		if strings.ContainsAny(p, `\`) {
			// A backslash is a path separator on some platforms and a literal
			// elsewhere. Refusing it keeps the meaning of a path identical on
			// every host Mirage might run on.
			return "", ErrInvalidPath
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return RootPath, nil
	}
	return strings.Join(out, "/"), nil
}

// IsRoot reports whether a cleaned path refers to the home directory itself.
func IsRoot(cleaned string) bool { return cleaned == RootPath }

// tempPrefix marks partially written files. They are never listed or indexed,
// so a client never sees a half-uploaded file and neither does anyone browsing
// the share over SMB.
const tempPrefix = ".mirage-tmp-"

// UploadDir holds in-progress chunked uploads, one directory per transfer.
//
// It lives inside the user's home rather than in server storage for two
// reasons: that is the volume with room for a partly uploaded 5 GB file, and it
// keeps one tenant's uploads inside their own directory like everything else.
const UploadDir = ".mirage-uploads"

// TrashDir holds deleted files until they are restored or expire.
//
// Inside the account's own directory, because a delete has to be a rename and a
// rename only works within one filesystem. That it is hidden from the index is
// not cosmetic: an indexed trash directory would sync every deleted file back
// to every device as a new file, which is the opposite of deleting it.
const TrashDir = ".mirage-trash"

// VersionsDir holds earlier copies of files that have been overwritten.
//
// Inside the account for the same reason the trash is, and hidden from the
// index for the same reason too: indexed, it would sync a copy of every earlier
// draft of everything to every device.
const VersionsDir = ".mirage-versions"

// ignoredNames are entries that exist on the filesystem but are not the user's
// files, and must not be indexed or shown to clients.
//
// Synology scatters @eaDir through every directory on the volume: it holds
// thumbnails and indexer metadata, is often large, and syncing it to every
// device would be pure noise. The others are DSM's own machinery in the same
// vein. Ignoring an entry only means Mirage does not index it - the directory
// itself is left entirely alone on disk.
var ignoredNames = map[string]bool{
	"@eaDir":    true, // thumbnails and indexer metadata, in every directory
	"#recycle":  true, // DSM recycle bin for the share
	"#snapshot": true, // DSM snapshot mount point
	"@tmp":      true,
	"@sharebin": true,
}

// IsInternal reports whether a directory entry should be hidden from clients
// and the index, either because it is Mirage's own bookkeeping or because it is
// filesystem machinery that is not the user's data.
func IsInternal(name string) bool {
	return strings.HasPrefix(name, tempPrefix) ||
		name == UploadDir || name == TrashDir || name == VersionsDir || ignoredNames[name]
}

// Excluder decides whether an entry is excluded by operator configuration.
//
// This is separate from IsInternal, which covers entries that are never anyone's
// data. Exclusions are a choice: a checkout's .svn directory or a node_modules
// tree can run to millions of tiny files that cost more to index than they are
// worth syncing, but they are still the user's files and somebody may want them.
type Excluder struct {
	patterns []string
}

// NewExcluder compiles exclusion patterns, which are matched against an entry's
// name - not its path - so "node_modules" excludes it at any depth.
//
// Patterns use filepath.Match syntax, so "*.tmp" works as well as a plain name.
func NewExcluder(patterns []string) (*Excluder, error) {
	for _, p := range patterns {
		if p == "" {
			return nil, errors.New("an exclusion pattern must not be empty")
		}
		if _, err := filepath.Match(p, "probe"); err != nil {
			return nil, fmt.Errorf("exclusion pattern %q is malformed: %w", p, err)
		}
	}
	return &Excluder{patterns: patterns}, nil
}

// Excludes reports whether an entry name is excluded.
func (e *Excluder) Excludes(name string) bool {
	if e == nil {
		return false
	}
	for _, p := range e.patterns {
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// Patterns returns the configured patterns, for reporting.
func (e *Excluder) Patterns() []string {
	if e == nil {
		return nil
	}
	return e.patterns
}

// Join appends a child name to a cleaned parent path.
func Join(parent, name string) string {
	if IsRoot(parent) {
		return name
	}
	return parent + "/" + name
}
