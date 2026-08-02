package mailbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/yarilomail/yarilo/pkg/dict"
)

// RFC 5464 METADATA entry names start with /private/ or /shared/, then a
// hierarchical path of "atom" components. Entries map to dict keys under
// per-folder GUID-keyed namespaces so state survives RENAME (folder names
// change, GUIDs do not).
//
// Key layouts produced by this package:
//
//	priv/box/<inbox_guid>/<vendor/yarilo/pvt/server/<entry>>   server-scope /private
//	shared/box/<inbox_guid>/<vendor/yarilo/pvt/server/<entry>> server-scope /shared
//	priv/box/<mbox_guid>/<entry>                               mailbox-scope /private
//	shared/box/<mbox_guid>/<entry>                             mailbox-scope /shared
//
// Server-scope entries (empty IMAP mailbox name) live under INBOX's GUID with
// a "vendor/yarilo/pvt/server/" prefix so they cannot collide with same-named
// mailbox attributes on INBOX itself.

// AttrScope distinguishes per-user-private from per-resource-shared attributes.
type AttrScope int

const (
	AttrPrivate AttrScope = iota
	AttrShared
)

// serverScopeVendorPrefix is added to the dict key for server-scope attributes
// so they cannot collide with same-named INBOX attributes.
const serverScopeVendorPrefix = "vendor/yarilo/pvt/server/"

// ParseAttrEntry splits a wire-format METADATA entry name like
// "/private/comment" into (scope, attrName), where attrName is everything
// after the scope prefix with the leading slash dropped:
//
//	/private/comment           -> (AttrPrivate, "comment")
//	/shared/admin              -> (AttrShared,  "admin")
//	/private/vendor/yarilo/abc -> (AttrPrivate, "vendor/yarilo/abc")
//
// Returns an error if the entry does not start with /private/ or /shared/, or
// is otherwise malformed. Re-checked here so callers that bypass the wire
// layer (tests, scripted scaffolding) get the same guarantees.
func ParseAttrEntry(entry string) (AttrScope, string, error) {
	switch {
	case strings.HasPrefix(entry, "/private/"):
		return AttrPrivate, strings.TrimPrefix(entry, "/private/"), nil
	case strings.HasPrefix(entry, "/shared/"):
		return AttrShared, strings.TrimPrefix(entry, "/shared/"), nil
	}
	return 0, "", fmt.Errorf("attribute entry %q must start with /private/ or /shared/", entry)
}

// AttrKey returns the dict key for a per-folder attribute on a
// personal-namespace mailbox:
//
//	priv/box/<hex(guid)>/<attrName>
//
// scope chooses the priv/ or shared/ namespace; folderGUID is the
// rename-stable Folder.GUID; attrName is the path-component (without leading
// slash) from ParseAttrEntry. The "box/" segment leaves room for future
// per-message or per-user scopes under priv/<other>/... without collision.
//
// For shared / public namespaces use SharedAttrKey instead.
func AttrKey(scope AttrScope, folderGUID [16]byte, attrName string) string {
	return scopePrefix(scope) + "box/" + hex.EncodeToString(folderGUID[:]) + "/" + attrName
}

// SharedAttrKey returns the dict key for a per-folder attribute on a shared or
// public namespace mailbox. The priv/ scope embeds a per-accessing-user
// dimension (userHash) so users cannot see each other's private annotations on
// the same shared folder; the shared/ scope is a single value visible to
// everyone. Hashing avoids raw usernames that may contain '/' or '%' clashing
// with dict path semantics.
func SharedAttrKey(scope AttrScope, folderGUID [16]byte, accessingUser, attrName string) string {
	base := scopePrefix(scope) + "box/" + hex.EncodeToString(folderGUID[:]) + "/"
	if scope == AttrPrivate {
		return base + "u-" + userHash(accessingUser) + "/" + attrName
	}
	return base + attrName
}

// userHash returns the first 16 hex chars (64 bits) of SHA-256(username), used
// as a per-user dimension in dict keys for shared/public namespaces.
func userHash(username string) string {
	sum := sha256.Sum256([]byte(username))
	return hex.EncodeToString(sum[:8])
}

// ServerAttrKey returns the dict key for a server-scope attribute (empty IMAP
// mailbox name), stored under INBOX's GUID with serverScopeVendorPrefix so it
// cannot collide with same-named INBOX mailbox attributes.
func ServerAttrKey(scope AttrScope, inboxGUID [16]byte, attrName string) string {
	return scopePrefix(scope) + "box/" + hex.EncodeToString(inboxGUID[:]) + "/" + serverScopeVendorPrefix + attrName
}

// AttrPrefix returns the dict-iteration path for all attributes of a
// personal-namespace folder under this scope (GETMETADATA DEPTH 1 / INFINITY).
func AttrPrefix(scope AttrScope, folderGUID [16]byte) string {
	return scopePrefix(scope) + "box/" + hex.EncodeToString(folderGUID[:]) + "/"
}

// SharedAttrPrefix returns the iteration path for a shared/public namespace
// folder. Mirrors SharedAttrKey: priv/ scope is per-user (includes the
// accessingUser hash subdir), shared/ scope is global to the folder.
func SharedAttrPrefix(scope AttrScope, folderGUID [16]byte, accessingUser string) string {
	base := scopePrefix(scope) + "box/" + hex.EncodeToString(folderGUID[:]) + "/"
	if scope == AttrPrivate {
		return base + "u-" + userHash(accessingUser) + "/"
	}
	return base
}

// ServerAttrPrefix returns the iteration path for server-scope attributes:
// AttrPrefix for the empty ("") mailbox.
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

// TrimAttrPrefix extracts the attrName from a full dict key given the
// scope+guid prefix the iterator used. Returns "" if key does not lie under
// prefix (defensive; callers should pre-filter).
func TrimAttrPrefix(key, prefix string) string {
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return strings.TrimPrefix(key, prefix)
}
