package imap

import (
	"fmt"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// SessionACL is implemented unconditionally on *session — go-imap
// detects it via interface assertion at capability and command
// dispatch time, so the assertion must succeed even when the
// operator has disabled ACL. The methods consult ACLEnabled and
// return a NO when the feature is off.

// rightsToIMAP converts the canonical-order yarilo right set into the
// RFC 4314 RightSet that go-imap surfaces on the wire. Byte-stable
// because mailbox.Rights is already a sorted single-letter string.
func rightsToIMAP(r mailbox.Rights) imaplib.RightSet {
	out := make(imaplib.RightSet, 0, len(r))
	for _, b := range []byte(string(r)) {
		out = append(out, imaplib.Right(b))
	}
	return out
}

// rightsFromIMAP converts an inbound RFC 4314 RightSet into canonical
// mailbox.Rights — dedupes, sorts, and expands obsolete c/d (PR B
// parser handles all three steps).
func rightsFromIMAP(rs imaplib.RightSet) (mailbox.Rights, error) {
	out, err := mailbox.ParseRights(string(rs))
	if err != nil {
		return "", err
	}
	return out, nil
}

// identifierToIMAP converts a stored identifier into the wire form
// surfaced in GETACL responses. The on-disk yarilo-acl format uses
// `user=` / `group=` / `group-override=` prefixes; the RFC 4314 wire
// does not. user names are surfaced bare; groups carry the
// `$`-prefix wire convention so a client SETACL round-trip preserves
// the type without ambiguity against a bare username.
//
// Returns "" for IDInvalid so a buggy caller produces a clearly
// malformed wire string rather than silently dropping the entry.
func identifierToIMAP(id mailbox.Identifier) imaplib.RightsIdentifier {
	switch id.Type {
	case mailbox.IDAnyone:
		return imaplib.RightsIdentifier("anyone")
	case mailbox.IDAuthenticated:
		return imaplib.RightsIdentifier("authenticated")
	case mailbox.IDOwner:
		return imaplib.RightsIdentifier("owner")
	case mailbox.IDUser:
		return imaplib.RightsIdentifier(id.Name)
	case mailbox.IDGroup:
		return imaplib.RightsIdentifier("$" + id.Name)
	case mailbox.IDGroupOverride:
		// No standard RFC 4314 wire form for group-override —
		// surface with disk prefix so the type round-trips. PR
		// F's admin tooling is the canonical place to manage
		// these.
		return imaplib.RightsIdentifier("group-override=" + id.Name)
	}
	return ""
}

// identifierFromIMAP parses an inbound RightsIdentifier into a stored
// identifier plus whether it is a negative-rights entry:
//
//   - a leading "-" marks a negative-rights identifier (RFC 4314 §3.1)
//   - "anyone" / "authenticated" / "owner" — special keywords
//   - "$<name>" — group wire convention → IDGroup
//   - "group-override=<name>" — disk-style passthrough (no RFC form)
//   - anything else — bare username → IDUser{Name: <value>}
func identifierFromIMAP(rid imaplib.RightsIdentifier) (mailbox.Identifier, bool, error) {
	s := string(rid)
	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = s[1:]
	}
	if s == "" {
		return mailbox.Identifier{}, false, fmt.Errorf("imap/acl: empty identifier")
	}
	switch s {
	case "anyone":
		return mailbox.Identifier{Type: mailbox.IDAnyone}, negative, nil
	case "authenticated":
		return mailbox.Identifier{Type: mailbox.IDAuthenticated}, negative, nil
	case "owner":
		return mailbox.Identifier{Type: mailbox.IDOwner}, negative, nil
	}
	if strings.HasPrefix(s, "$") {
		name := strings.TrimPrefix(s, "$")
		if name == "" {
			return mailbox.Identifier{}, false, fmt.Errorf("imap/acl: empty group identifier")
		}
		return mailbox.Identifier{Type: mailbox.IDGroup, Name: name}, negative, nil
	}
	if strings.HasPrefix(s, "group-override=") {
		id, err := mailbox.ParseIdentifier(s)
		return id, negative, err
	}
	return mailbox.Identifier{Type: mailbox.IDUser, Name: s}, negative, nil
}

// aclSurfaceEntries skips negative entries when surfacing the ACL on
// the GETACL wire — RFC 4314 §3.6 says negative entries are
// represented by their own line with the identifier prefixed by '-'.
// PR C surfaces them in the same fashion (one entry per disk entry,
// negatives carrying the '-' prefix in the wire identifier) so a
// client doing a full GETACL → SETACL round-trip preserves them.
func aclSurfaceEntries(acl mailbox.ACL) []imaplib.ACLEntry {
	out := make([]imaplib.ACLEntry, 0, len(acl))
	for _, e := range acl {
		id := identifierToIMAP(e.Identifier)
		if e.Negative {
			id = imaplib.RightsIdentifier("-" + string(id))
		}
		out = append(out, imaplib.ACLEntry{
			Identifier: id,
			Rights:     rightsToIMAP(e.Rights),
		})
	}
	return out
}

// resolveACLHandle routes a wire mailbox name to its namespace ACL
// handle. Returns a NO error for declared-but-unimplemented
// namespaces and for handles without an ACL store (defensive — every
// implemented namespace has one).
func (s *session) resolveACLHandle(folder string) (*nsHandle, string, error) {
	h, rel, err := s.dispatch(folder)
	if err != nil {
		return nil, "", err
	}
	if !h.implemented() {
		return nil, "", &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Namespace not implemented"}
	}
	if h.acl == nil {
		return nil, "", &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL store not wired for this namespace"}
	}
	return h, rel, nil
}

// requireACLEnabled returns a NO error when the operator has disabled
// the ACL extension. Every SessionACL method calls this first.
func (s *session) requireACLEnabled() error {
	if !s.srv.opts.ACLEnabled {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL extension disabled by operator"}
	}
	return nil
}

// adminCheckPRc is the PR-C-scoped admin check on SETACL/DELETEACL:
// allow when the accessing user matches the namespace user (the
// "owner" of personal namespace; for shared/public this is whatever
// the synthetic UserInfo.Username is) OR when an explicit user=
// entry for the accessing user carries the 'a' right.
//
// Full RFC 4314 admin resolution (inheritance walk, owner identifier
// detection, negative-rights merge) lands in PR D / E. The narrower
// rule here is enough for owner-only flows to round-trip without
// granting non-owners write access to ACLs.
func (s *session) adminCheckPRc(h *nsHandle, current mailbox.ACL) error {
	if s.userInfo.Username == h.userInfo.Username {
		return nil
	}
	want := imaplib.RightAdminister
	for _, e := range current {
		if e.Negative {
			continue
		}
		if e.Identifier.Type == mailbox.IDUser && e.Identifier.Name == s.userInfo.Username {
			if strings.ContainsRune(string(e.Rights), rune(want)) {
				return nil
			}
		}
	}
	return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Permission denied: admin right required"}
}

// GetACL implements imapserver.SessionACL.
//
// Surfaces stored ACL entries. When the namespace owner has no explicit
// positive entry, an implicit owner=FullRights entry is prepended so
// that the owner is always visible in GETACL responses.
func (s *session) GetACL(folder string) (*imaplib.GetACLData, error) {
	if err := s.requireACLEnabled(); err != nil {
		return nil, err
	}
	h, rel, err := s.resolveACLHandle(folder)
	if err != nil {
		return nil, err
	}
	stored, err := h.acl.Get(rel)
	if err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL read failed: " + err.Error()}
	}
	ownerName := h.userInfo.Username
	ownerHasExplicit := false
	for _, e := range stored {
		if !e.Negative && e.Identifier.Type == mailbox.IDUser && e.Identifier.Name == ownerName {
			ownerHasExplicit = true
			break
		}
	}
	entries := aclSurfaceEntries(stored)
	if !ownerHasExplicit {
		implicit := imaplib.ACLEntry{
			Identifier: imaplib.RightsIdentifier(ownerName),
			Rights:     rightsToIMAP(mailbox.FullRights),
		}
		entries = append([]imaplib.ACLEntry{implicit}, entries...)
	}
	return &imaplib.GetACLData{
		Mailbox: folder,
		ACL:     entries,
	}, nil
}

// MyRights implements imapserver.SessionACL.
//
// Returns the effective rights for the current user on the named mailbox.
// The namespace owner always receives FullRights regardless of stored
// entries (RFC 4314 §4 implicit owner grant). Non-owners receive the
// union of explicit user= entries; group= / owner / inheritance walk
// land in PR D/E.
func (s *session) MyRights(folder string) (*imaplib.MyRightsData, error) {
	if err := s.requireACLEnabled(); err != nil {
		return nil, err
	}
	h, rel, err := s.resolveACLHandle(folder)
	if err != nil {
		return nil, err
	}
	// Resolve the full effective rights the same way enforcement does —
	// ancestor inheritance, the global ACL and acl_defaults_from_inbox —
	// so MYRIGHTS matches what SELECT/APPEND/etc. actually allow.
	rights, err := h.acl.EffectiveFor(rel, s.userInfo.Username, s.userInfo.Groups, s.isOwner(h), byte(h.spec.Separator))
	if err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL read failed: " + err.Error()}
	}
	return &imaplib.MyRightsData{
		Mailbox: folder,
		Rights:  rightsToIMAP(rights),
	}, nil
}

// ListRights implements imapserver.SessionACL (RFC 4314 §3.7): the rights that
// can be granted to identifier on folder. The mailbox owner is always granted
// every right (required set = all, nothing optional); for any other identifier
// no right is implied and each standard right is individually grantable
// (required empty, one optional element per right letter).
func (s *session) ListRights(folder string, identifier imaplib.RightsIdentifier) (*imaplib.ListRightsData, error) {
	if err := s.requireACLEnabled(); err != nil {
		return nil, err
	}
	h, rel, err := s.resolveACLHandle(folder)
	if err != nil {
		return nil, err
	}
	id, _, err := identifierFromIMAP(identifier)
	if err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
	}
	// Probe the folder exists by attempting a load — a nil error here
	// (whether the file exists or not) confirms the mailbox is
	// reachable in this namespace.
	if _, err := h.acl.Get(rel); err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL read failed: " + err.Error()}
	}
	// The mailbox owner (the "owner" keyword, or the owning user of a personal
	// namespace) implicitly holds all rights.
	isOwnerID := id.Type == mailbox.IDOwner ||
		(h.spec.Type == NamespacePersonal && id.Type == mailbox.IDUser && id.Name == s.userInfo.Username)
	if isOwnerID {
		return &imaplib.ListRightsData{
			Mailbox:        folder,
			Identifier:     identifier,
			RequiredRights: imaplib.RightSet(mailbox.FullRights),
		}, nil
	}
	optional := make([]imaplib.RightSet, 0, len(mailbox.FullRights))
	for _, r := range mailbox.FullRights {
		optional = append(optional, imaplib.RightSet(string(r)))
	}
	return &imaplib.ListRightsData{
		Mailbox:        folder,
		Identifier:     identifier,
		RequiredRights: imaplib.RightSet{},
		OptionalRights: optional,
	}, nil
}

// SetACL implements imapserver.SessionACL.
func (s *session) SetACL(folder string, identifier imaplib.RightsIdentifier, modification imaplib.RightModification, rights imaplib.RightSet) error {
	if err := s.requireACLEnabled(); err != nil {
		return err
	}
	h, rel, err := s.resolveACLHandle(folder)
	if err != nil {
		return err
	}
	id, negative, err := identifierFromIMAP(identifier)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
	}
	parsed, err := rightsFromIMAP(rights)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
	}
	return h.acl.Update(rel, func(cur mailbox.ACL) (mailbox.ACL, error) {
		if cur == nil {
			cur = mailbox.ACL{}
		}
		if err := s.adminCheckPRc(h, cur); err != nil {
			return nil, err
		}
		return applySetACL(cur, id, negative, modification, parsed), nil
	})
}

// DeleteACL implements imapserver.SessionACL — equivalent to SetACL
// with Replace and empty rights (RFC 4314 §3.2).
func (s *session) DeleteACL(folder string, identifier imaplib.RightsIdentifier) error {
	if err := s.requireACLEnabled(); err != nil {
		return err
	}
	h, rel, err := s.resolveACLHandle(folder)
	if err != nil {
		return err
	}
	id, negative, err := identifierFromIMAP(identifier)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
	}
	return h.acl.Update(rel, func(cur mailbox.ACL) (mailbox.ACL, error) {
		if cur == nil {
			return nil, nil
		}
		if err := s.adminCheckPRc(h, cur); err != nil {
			return nil, err
		}
		return dropIdentifier(cur, id, negative), nil
	})
}

// applySetACL is the in-memory transform a SETACL command performs on the
// stored entry set. Identifier matching is by Type+Name AND the negative flag,
// so `SETACL mbox -bob r` (RFC 4314 §3.1 negative rights) modifies bob's
// negative entry independently of his positive one.
//
// Replace with empty rights drops the matching entry (RFC 4314 §3.1: "If a
// SETACL is performed with an empty rights argument, the existing rights are
// deleted").
func applySetACL(cur mailbox.ACL, id mailbox.Identifier, negative bool, mod imaplib.RightModification, rights mailbox.Rights) mailbox.ACL {
	idx := -1
	for i, e := range cur {
		if e.Negative == negative && e.Identifier == id {
			idx = i
			break
		}
	}

	switch mod {
	case imaplib.RightModificationReplace:
		if rights == "" {
			if idx >= 0 {
				return append(cur[:idx], cur[idx+1:]...)
			}
			return cur
		}
		if idx >= 0 {
			cur[idx].Rights = rights
			return cur
		}
		return append(cur, mailbox.Entry{Identifier: id, Rights: rights, Negative: negative})
	case imaplib.RightModificationAdd:
		if idx >= 0 {
			cur[idx].Rights = cur[idx].Rights.Add(rights)
			return cur
		}
		return append(cur, mailbox.Entry{Identifier: id, Rights: rights, Negative: negative})
	case imaplib.RightModificationRemove:
		if idx >= 0 {
			cur[idx].Rights = cur[idx].Rights.Remove(rights)
			if cur[idx].Rights == "" {
				return append(cur[:idx], cur[idx+1:]...)
			}
		}
		return cur
	}
	return cur
}

// dropIdentifier removes the entry for the given identifier and negativity —
// DELETEACL of `bob` drops bob's positive entry, `-bob` drops his negative one
// (RFC 4314 §3.2).
func dropIdentifier(cur mailbox.ACL, id mailbox.Identifier, negative bool) mailbox.ACL {
	out := cur[:0]
	for _, e := range cur {
		if e.Negative == negative && e.Identifier == id {
			continue
		}
		out = append(out, e)
	}
	return out
}
