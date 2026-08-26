// Package account holds the rules an account must satisfy, wherever it was
// defined.
//
// Accounts can now come from two places: the config file, or the admin page.
// Both must enforce the same constraints, and one of those constraints is a
// tenant-isolation invariant rather than a matter of taste. Keeping the rules
// here means the admin page cannot accidentally permit something the config
// file rejects.
package account

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// usernameRe matches usernames safe to embed in a WebDAV URL path segment.
// A slash would let a name escape its segment; the rest is conservatism.
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// Mapping is an account's identity and the directory backing it.
type Mapping struct {
	Username string
	Home     string
}

// ValidateUsername checks that a name is usable.
func ValidateUsername(username string) error {
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("username %q must be 1-64 characters of letters, digits, dot, underscore, at-sign or hyphen", username)
	}
	// A username is placed straight into the WebDAV path as
	// /remote.php/dav/files/<username>/..., so a name made only of dots is a
	// path traversal waiting to be normalised by something along the way. The
	// character class above permits dots for names like "first.last", which
	// makes "." and ".." members of it by accident.
	if strings.Trim(username, ".") == "" {
		return fmt.Errorf("username %q is not allowed: it would be read as a path segment", username)
	}
	return nil
}

// ValidateHome checks that a home path is usable, returning it cleaned.
func ValidateHome(home string) (string, error) {
	if home == "" {
		return "", fmt.Errorf("a home directory is required")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("home %q must be an absolute path", home)
	}
	return filepath.Clean(home), nil
}

// ValidateOwnership checks a uid and gid.
func ValidateOwnership(uid, gid int) error {
	if uid < 0 {
		return fmt.Errorf("uid must not be negative")
	}
	if gid < 0 {
		return fmt.Errorf("gid must not be negative")
	}
	return nil
}

// CheckConflicts rejects a mapping that collides with an existing one.
//
// Two accounts sharing a directory, or one sitting inside another, both break
// tenant isolation - and the nested case cannot be caught anywhere else. Path
// confinement stops an account escaping its own directory, but if one account's
// directory legitimately contains another's, nothing is escaping and the outer
// account simply owns the inner one's files.
func CheckConflicts(candidate Mapping, existing []Mapping) error {
	for _, other := range existing {
		if strings.EqualFold(other.Username, candidate.Username) {
			return fmt.Errorf("an account named %q already exists", other.Username)
		}
		switch {
		case other.Home == candidate.Home:
			return fmt.Errorf("%q already uses %s; each account needs a private directory",
				other.Username, candidate.Home)
		case isWithin(candidate.Home, other.Home):
			return fmt.Errorf("%s is inside %s, which belongs to %q, who would be able to see these files",
				candidate.Home, other.Home, other.Username)
		case isWithin(other.Home, candidate.Home):
			return fmt.Errorf("%s contains %s, which belongs to %q, whose files would be visible here",
				candidate.Home, other.Home, other.Username)
		}
	}
	return nil
}

// isWithin reports whether child lies inside parent. Both must be cleaned
// absolute paths.
func isWithin(child, parent string) bool {
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}
