package dav

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/poitch/mirage/internal/store"
)

// Property names Mirage understands.
var (
	propGetLastModified  = PropName{NSDav, "getlastmodified"}
	propGetContentLength = PropName{NSDav, "getcontentlength"}
	propGetContentType   = PropName{NSDav, "getcontenttype"}
	propResourceType     = PropName{NSDav, "resourcetype"}
	propGetETag          = PropName{NSDav, "getetag"}
	propDisplayName      = PropName{NSDav, "displayname"}
	propQuotaAvailable   = PropName{NSDav, "quota-available-bytes"}
	propQuotaUsed        = PropName{NSDav, "quota-used-bytes"}
	propOCID             = PropName{NSOwnCloud, "id"}
	propOCFileID         = PropName{NSOwnCloud, "fileid"}
	propOCPermissions    = PropName{NSOwnCloud, "permissions"}
	propOCSize           = PropName{NSOwnCloud, "size"}
	propOCOwnerID        = PropName{NSOwnCloud, "owner-id"}
	propOCOwnerName      = PropName{NSOwnCloud, "owner-display-name"}
	propOCChecksums      = PropName{NSOwnCloud, "checksums"}
	propOCFavorite       = PropName{NSOwnCloud, "favorite"}
	propNCHasPreview     = PropName{NSNextcloud, "has-preview"}
	propNCIsEncrypted    = PropName{NSNextcloud, "is-encrypted"}
	propNCMountType      = PropName{NSNextcloud, "mount-type"}
)

// allPropNames is what an allprop request expands to. It is deliberately the
// set clients actually read, not everything conceivable: each extra property is
// work on every entry of every directory listing.
var allPropNames = []PropName{
	propGetLastModified, propGetContentLength, propGetContentType,
	propResourceType, propGetETag, propDisplayName,
	propQuotaAvailable, propQuotaUsed,
	propOCID, propOCFileID, propOCPermissions, propOCSize,
	propOCOwnerID, propOCOwnerName, propOCChecksums, propOCFavorite,
	propNCHasPreview, propNCIsEncrypted, propNCMountType,
}

// quotaUnlimited is the sentinel WebDAV uses for an account with no limit.
const quotaUnlimited = -3

// resourceContext carries everything property resolution needs about one entry.
type resourceContext struct {
	node       store.Node
	user       store.User
	instanceID string
	usage      int64
	readOnly   bool
	// hasPreview reports whether a thumbnail can be produced for this entry.
	//
	// Clients read nc:has-preview and only ask for a picture when it says yes,
	// so reporting false means the preview endpoint is never called at all -
	// which is what made previews appear not to work even once they existed.
	hasPreview bool
}

// permissions renders the oc:permissions letters for an entry.
//
// The letters describe what the client may do, and a client believes them: if
// Mirage claims a file is writable before writes are implemented, the client
// offers to edit it and the operation then fails. So the set is derived from
// what the server can actually honour right now.
func (rc resourceContext) permissions() string {
	if rc.readOnly {
		return "G" // readable only
	}
	if rc.node.IsDir {
		// G readable, D deletable, N renameable, V moveable,
		// C may contain new files, K may contain new directories.
		return "GDNVCK"
	}
	return "GDNVW" // ... W writable
}

// resolve returns the rendered value of one property, or false if this resource
// does not have it.
func (rc resourceContext) resolve(name PropName) (string, bool) {
	n := rc.node
	switch name {
	case propGetLastModified:
		return escapeText(n.MTime.UTC().Format(http.TimeFormat)), true

	case propGetContentLength:
		// Directories have no content length. Reporting one confuses clients
		// into treating a collection as a file.
		if n.IsDir {
			return "", false
		}
		return strconv.FormatInt(n.Size, 10), true

	case propGetContentType:
		if n.IsDir {
			return "", false
		}
		return escapeText(n.ContentType), true

	case propResourceType:
		if n.IsDir {
			return "<d:collection/>", true
		}
		return "", true

	case propGetETag:
		// ETags travel quoted, per RFC 7232.
		return escapeText(`"` + n.ETag + `"`), true

	case propDisplayName:
		name := n.Name
		if n.Path == "." {
			name = rc.user.Username
		}
		return escapeText(name), true

	case propQuotaUsed:
		if !n.IsDir {
			return "", false
		}
		return strconv.FormatInt(n.Size, 10), true

	case propQuotaAvailable:
		if !n.IsDir {
			return "", false
		}
		if rc.user.Quota <= 0 {
			return strconv.Itoa(quotaUnlimited), true
		}
		free := rc.user.Quota - rc.usage
		if free < 0 {
			free = 0
		}
		return strconv.FormatInt(free, 10), true

	case propOCID:
		// Nextcloud's oc:id is the file ID zero-padded to eight digits with the
		// instance ID appended, and some clients parse exactly that shape.
		return escapeText(fmt.Sprintf("%08d%s", n.ID, rc.instanceID)), true

	case propOCFileID:
		return strconv.FormatInt(n.ID, 10), true

	case propOCPermissions:
		return rc.permissions(), true

	case propOCSize:
		// Unlike getcontentlength this is defined for directories, where it is
		// the total size of everything beneath.
		return strconv.FormatInt(n.Size, 10), true

	case propOCOwnerID:
		return escapeText(rc.user.Username), true

	case propOCOwnerName:
		return escapeText(rc.user.DisplayName), true

	case propOCChecksums:
		// Announced as present but empty: Mirage does not store checksums, and
		// omitting the element makes some clients retry the PROPFIND.
		return "", true

	case propOCFavorite:
		return "0", true

	case propNCHasPreview:
		if rc.hasPreview {
			return "true", true
		}
		return "false", true

	case propNCIsEncrypted:
		return "0", true

	case propNCMountType:
		// Empty means an ordinary directory rather than an external mount.
		return "", true
	}
	return "", false
}

// resolveAll splits the requested property names into those this resource has
// and those it does not.
func (rc resourceContext) resolveAll(names []PropName) (found []prop, missing []PropName) {
	found = make([]prop, 0, len(names))
	for _, name := range names {
		value, ok := rc.resolve(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		found = append(found, prop{Name: name, Value: value})
	}
	return found, missing
}
