package mailbox

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// RFC 5464 METADATA entry names start with /private/ or /shared/, then a
// hierarchical path of "atom" components. Internally we map those entries
// to dict keys under per-folder GUID-keyed namespaces, so RENAME-stable
// state is preserved (folder names change, GUIDs do not).
//
// Key layouts produced by this package:
//
//	priv/box/<inbox_guid>/<vendor/yarilo/pvt/server/<entry>>   server-scope /private
//	shared/box/<inbox_guid>/<vendor/yarilo/pvt/server/<entry>> server-scope /shared
//	priv/box/<mbox_guid>/<entry>                               mailbox-scope /private
//	shared/box/<mbox_guid>/<entry>                             mailbox-scope /shared
//
// Server-scope entries (those whose IMAP mailbox name is the empty string)
// live under INBOX's GUID with a "vendor/yarilo/pvt/server/" prefix so a
// server-scope /private/comment cannot collide with the same-named
// mailbox attribute on INBOX itself.

// AttrScope distinguishes per-user-private from per-resource-shared attributes.
type AttrScope int

const (
	AttrPrivate AttrScope = iota
	AttrShared
)

// serverScopeVendorPrefix is appended to the dict key for server-scope
// attributes so they cannot collide with same-named INBOX attributes.
// "vendor/yarilo/pvt/server/" matches the structure other operators use
// for vendor-namespaced server metadata.
const serverScopeVendorPrefix = "vendor/yarilo/pvt/server/"

// ParseAttrEntry splits a wire-format METADATA entry name like
// "/private/comment" into (scope, attrName). The attrName is everything
// after the scope prefix, with the leading slash dropped:
//
//	/private/comment           → (AttrPrivate, "comment")
//	/shared/admin              → (AttrShared,  "admin")
//	/private/vendor/yarilo/abc → (AttrPrivate, "vendor/yarilo/abc")
//
// Returns an error if the entry does not start with /private/ or /shared/,
// or is otherwise malformed. The fork-side ValidateMetadataEntry already
// catches most of this on parse; we re-check here so callers that bypass
// the wire layer (tests, scripted scaffolding) still get the same
// guarantees.
func ParseAttrEntry(entry string) (AttrScope, string, error) {
	switch {
	case strings.HasPrefix(entry, "/private/"):
		return AttrPrivate, strings.TrimPrefix(entry, "/private/"), nil
	case strings.HasPrefix(entry, "/shared/"):
		return AttrShared, strings.TrimPrefix(entry, "/shared/"), nil
	}
	return 0, "", fmt.Errorf("attribute entry %q must start with /private/ or /shared/", entry)
}

// AttrKey returns the dict key for a per-folder attribute.
//
// scope chooses the priv/ or shared/ namespace; folderGUID is the
// rename-stable identifier of the target folder (Folder.GUID); attrName
// is the path-component (without leading slash) returned by
// ParseAttrEntry.
//
// The resulting key is of the form:
//
//	priv/box/<hex(guid)>/<attrName>
//
// The "box/" segment exists so a future extension (e.g. per-message or
// per-user-scoped attributes) can co-exist under priv/<other>/... without
// colliding with mailbox attributes.
func AttrKey(scope AttrScope, folderGUID [16]byte, attrName string) string {
	return scopePrefix(scope) + "box/" + hex.EncodeToString(folderGUID[:]) + "/" + attrName
}

// ServerAttrKey returns the dict key for a server-scope attribute.
//
// Server-scope attributes (those whose IMAP mailbox name is empty) are
// stored under INBOX's GUID with the serverScopeVendorPrefix added so
// they cannot collide with same-named INBOX mailbox attributes.
func ServerAttrKey(scope AttrScope, inboxGUID [16]byte, attrName string) string {
	return scopePrefix(scope) + "box/" + hex.EncodeToString(inboxGUID[:]) + "/" + serverScopeVendorPrefix + attrName
}

// AttrPrefix returns the dict-iteration path for "all attributes of this
// folder under this scope". Useful for METADATA GETMETADATA with
// DEPTH 1 / DEPTH INFINITY.
func AttrPrefix(scope AttrScope, folderGUID [16]byte) string {
	return scopePrefix(scope) + "box/" + hex.EncodeToString(folderGUID[:]) + "/"
}

// ServerAttrPrefix returns the iteration path for server-scope attributes
// — the equivalent of AttrPrefix for "" (empty) mailbox.
func ServerAttrPrefix(scope AttrScope, inboxGUID [16]byte) string {
	return scopePrefix(scope) + "box/" + hex.EncodeToString(inboxGUID[:]) + "/" + serverScopeVendorPrefix
}

func scopePrefix(s AttrScope) string {
	if s == AttrShared {
		return dict.PathShared
	}
	return dict.PathPrivate
}

// FormatAttrEntry returns the wire-format entry name for a (scope, attrName)
// pair: the inverse of ParseAttrEntry. Used when iterating dict keys back
// into a GETMETADATA response map.
func FormatAttrEntry(scope AttrScope, attrName string) string {
	if scope == AttrShared {
		return "/shared/" + attrName
	}
	return "/private/" + attrName
}

// TrimAttrPrefix extracts the attrName from a full dict key, given the
// scope+guid prefix the iterator used. Returns "" if key does not lie
// under prefix (defensive — callers should pre-filter).
func TrimAttrPrefix(key, prefix string) string {
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return strings.TrimPrefix(key, prefix)
}
