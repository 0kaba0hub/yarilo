package imap

import (
	"fmt"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SessionACL is implemented unconditionally on *session: go-imap detects it via
// interface assertion at capability and dispatch time, so the assertion must
// succeed even when ACL is disabled. The methods consult ACLEnabled and return
// a NO when the feature is off.

// rightsToIMAP converts a canonical-order right set into the RFC 4314 RightSet
// go-imap surfaces on the wire.
func rightsToIMAP(r mailbox.Rights) imaplib.RightSet {
	out := make(imaplib.RightSet, 0, len(r))
	for _, b := range []byte(string(r)) {
		out = append(out, imaplib.Right(b))
	}
	return out
}

// rightsFromIMAP converts an inbound RFC 4314 RightSet into canonical
// mailbox.Rights (dedupe, sort, expand obsolete c/d).
func rightsFromIMAP(rs imaplib.RightSet) (mailbox.Rights, error) {
	out, err := mailbox.ParseRights(string(rs))
	if err != nil {
		return "", err
	}
	return out, nil
}

// identifierToIMAP converts a stored identifier into its GETACL wire form.
// Users are surfaced bare; groups carry the `$`-prefix wire convention so a
// SETACL round-trip preserves the type unambiguously against a bare username.
// Returns "" for IDInvalid so a buggy caller emits a clearly malformed string
// rather than silently dropping the entry.
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
		// No standard RFC 4314 wire form for group-override; surface with
		// the disk prefix so the type round-trips.
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

// aclSurfaceEntries emits one wire entry per disk entry. Per RFC 4314 §3.6 a
// negative entry is its own line with the identifier prefixed by '-', so a full
// GETACL → SETACL round-trip preserves it.
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

// resolveACLHandle routes a wire mailbox name to its namespace ACL handle.
// Returns a NO for declared-but-unimplemented namespaces and for handles
// without an ACL store.
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
	// RFC 4314 3.3: the ACL commands answer NO for a mailbox that is not
	// there. Nothing on this path checked, so every one of them answered for
	// any name at all -- GETACL reported full rights on a mailbox that does
	// not exist, which makes the command useless for auditing, the one thing
	// it is for (#1075).
	//
	// Checked here rather than in each of the five commands: they already
	// share this resolver, and five copies is how one of them ends up without.
	exists, err := h.box.FolderExists(rel)
	if err != nil {
		return nil, "", err
	}
	if !exists {
		return nil, "", &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
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

// adminCheckPRc authorises SETACL/DELETEACL: allow when the accessing user is
// the namespace user, or when an explicit user= entry for them carries the 'a'
// (administer) right.
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
// Returns the effective rights for the current user on the named mailbox. The
// namespace owner always receives FullRights (RFC 4314 §4 implicit owner
// grant); non-owners receive the resolved effective rights.
func (s *session) MyRights(folder string) (*imaplib.MyRightsData, error) {
	if err := s.requireACLEnabled(); err != nil {
		return nil, err
	}
	h, rel, err := s.resolveACLHandle(folder)
	if err != nil {
		return nil, err
	}
	// Resolve effective rights the same way enforcement does (ancestor
	// inheritance, global ACL, acl_defaults_from_inbox) so MYRIGHTS matches what
	// SELECT/APPEND/etc. actually allow.
	aclUser, aclGroups := s.userInfo.ACLIdentity()
	rights, err := h.acl.EffectiveFor(rel, aclUser, aclGroups, s.isOwner(h), byte(h.spec.Separator))
	if err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL read failed: " + err.Error()}
	}
	return &imaplib.MyRightsData{
		Mailbox: folder,
		Rights:  rightsToIMAP(rights),
	}, nil
}

// listRightsOptional is the RFC 4314 §3.7 "optional rights" set advertised by
// LISTRIGHTS: every standard right plus the two obsolete compound rights
// (c = k+x create, d = e+t delete), each individually grantable. No right is
// ever implied for any identifier.
const listRightsOptional = "lrwstpiekxacd"

// ListRights implements imapserver.SessionACL (RFC 4314 §3.7): the rights that
// can be granted to identifier on folder. No right is implied for any
// identifier (required set empty); every right is individually grantable.
func (s *session) ListRights(folder string, identifier imaplib.RightsIdentifier) (*imaplib.ListRightsData, error) {
	if err := s.requireACLEnabled(); err != nil {
		return nil, err
	}
	h, rel, err := s.resolveACLHandle(folder)
	if err != nil {
		return nil, err
	}
	if _, _, err := identifierFromIMAP(identifier); err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
	}
	// Probe reachability: a nil error (file present or not) confirms the
	// mailbox is reachable in this namespace.
	if _, err := h.acl.Get(rel); err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL read failed: " + err.Error()}
	}
	optional := make([]imaplib.RightSet, 0, len(listRightsOptional))
	for _, r := range listRightsOptional {
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
