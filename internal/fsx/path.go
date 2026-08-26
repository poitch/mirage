// Package fsx provides per-user filesystem access confined to that user's home
// directory.
package fsx

import (
	"errors"
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

// IsInternal reports whether a directory entry is Mirage's own bookkeeping and
// should be hidden from clients and the index.
func IsInternal(name string) bool {
	return strings.HasPrefix(name, tempPrefix)
}

// Join appends a child name to a cleaned parent path.
func Join(parent, name string) string {
	if IsRoot(parent) {
		return name
	}
	return parent + "/" + name
}
