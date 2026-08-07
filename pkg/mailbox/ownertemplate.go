package mailbox

import (
	"fmt"
	"strings"
)

// OwnerVar is the owner variable a namespace prefix carries to become
// owner-templated: the segment filling its slot names the owner of the instance
// (docs/OWNER_SHARED_NS.md 3.1). v1 supports %u (the full username); %n / %d
// split-slot forms are a documented follow-up.
const OwnerVar = "%u"

// PrefixIsOwnerTemplated reports whether a namespace prefix carries the owner
// variable. One definition, shared by config validation and the IMAP resolver,
// so the two cannot disagree about what "owner-templated" means.
func PrefixIsOwnerTemplated(prefix string) bool {
	return strings.Contains(prefix, OwnerVar)
}

// ExtractOwner parses a wire mailbox name under an owner-templated prefix into
// the owner it names and the folder relative to that owner's store. sep is the
// namespace separator (0 defaults to '/').
//
//	prefix "user/%u/", "user/alice/Sent" -> "alice", "Sent"
//	                   "user/alice"       -> "alice", "INBOX"
//
// ok is false when the name is not under the prefix, when the owner segment is
// malformed (empty, "." or ".."), or when the post-variable literal is anything
// but the separator (the v1 limitation). A false result must resolve to no
// owner, never to the requesting user -- the honesty isOwner depends on.
func ExtractOwner(prefix string, sep byte, name string) (owner, rel string, ok bool) {
	if !PrefixIsOwnerTemplated(prefix) {
		return "", "", false
	}
	before, after, _ := strings.Cut(prefix, OwnerVar)
	if !strings.HasPrefix(name, before) {
		return "", "", false
	}
	s := "/"
	if sep != 0 {
		s = string(sep)
	}
	if after != "" && after != s {
		return "", "", false
	}
	seg, tail, hasSep := strings.Cut(name[len(before):], s)
	if !validOwnerSegment(seg) {
		return "", "", false
	}
	if !hasSep || tail == "" {
		return seg, "INBOX", true
	}
	return seg, tail, true
}

// validOwnerSegment rejects a segment that cannot be a username: empty, or the
// filesystem-relative "." / "..". The userdb lookup fails closed for the rest.
func validOwnerSegment(seg string) bool {
	switch seg {
	case "", ".", "..":
		return false
	}
	return true
}

// StampOwnerLocation builds the storage identity for the owner of an
// owner-templated namespace instance from the owner's ALREADY-RESOLVED userdb
// UserInfo. The lookup that produces owner stays with each caller (the IMAP
// on-demand cache, the admin AuthClient) -- one producer here, not a lookup
// parameter each caller fills differently.
//
// Precedence (docs/OWNER_SHARED_NS.md 3.3): the owner's userdb decides the
// driver and the root (Home/MailPath); the namespace location fills only fields
// the userdb left empty. Overwriting the root with the template would point a
// per-user driver at a path it does not match -- the parallel-tree bug. base,
// when non-nil, supplies the deployment-wide storage-name form.
func StampOwnerLocation(owner, base *UserInfo, location string, sep byte) (*UserInfo, error) {
	if owner == nil {
		return nil, fmt.Errorf("mailbox: nil owner UserInfo")
	}
	ui := *owner // copy: callers may hold owner in a cache
	out := &ui

	loc, ok, err := ParseLocation(location, out)
	if err != nil {
		return nil, fmt.Errorf("mailbox: owner location %q: %w", location, err)
	}
	if !ok || loc.Path == "" {
		return nil, fmt.Errorf("mailbox: owner location %q resolved to no path", location)
	}
	fillIfEmpty(&out.MailPath, loc.Path)
	fillIfEmpty(&out.Home, loc.Path)
	out.Separator = string(sep)
	fillIfEmpty(&out.IndexDir, loc.IndexDir)
	fillIfEmpty(&out.VolatileDir, loc.VolatileDir)
	fillIfEmpty(&out.ControlDir, loc.ControlDir)
	fillIfEmpty(&out.AltDir, loc.AltDir)
	if base != nil {
		out.StorageEscapeChar = base.StorageEscapeChar
		out.SkipNFCNormalize = base.SkipNFCNormalize
	}
	return out, nil
}

func fillIfEmpty(dst *string, src string) {
	if *dst == "" {
		*dst = src
	}
}
