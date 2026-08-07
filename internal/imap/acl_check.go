package imap

import (
	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ACL enforcement entry points. The require* helpers resolve a folder name
// to its namespace handle, compute the accessing user's effective rights,
// and return a NO error with RFC 5530 NOPERM code when the requested right
// is absent. When ACLEnabled is false every helper returns nil.

// isOwner reports whether the accessing session owns the mailbox in the
// given namespace handle. Owner == personal namespace: every mailbox in the
// user's own home is owned by them regardless of any explicit `owner` ACL
// entry. Shared/public mailboxes are never auto-owned.
func (s *session) isOwner(h *nsHandle) bool {
	return h.spec.Type == NamespacePersonal
}

// aclEnforced reports whether ACL checks fire for this handle: ACL enabled
// server-wide AND the namespace does not carry acl_ignore.
func (s *session) aclEnforced(h *nsHandle) bool {
	return s.srv.opts.ACLEnabled && h != nil && !h.spec.IgnoreACL
}

// effectiveRights resolves the accessing user's effective rights on folder
// under h, walking to the first ancestor with an explicit ACL when folder
// has none (see acl.Store.EffectiveFor). The namespace separator drives the
// walk; sep == 0 disables it.
func (s *session) effectiveRights(h *nsHandle, folder string) (mailbox.Rights, error) {
	aclUser, aclGroups := s.userInfo.ACLIdentity()
	return h.acl.EffectiveFor(folder, aclUser, aclGroups, s.isOwner(h), byte(h.spec.Separator))
}

// requireRight returns nil when the accessing user holds right on folder
// under h, or a NO/NOPERM error otherwise. ACL store errors (parse/IO)
// surface as NO rather than a silent deny, so operators can debug from the
// client transcript.
func (s *session) requireRight(h *nsHandle, folder string, right rune) error {
	if !s.aclEnforced(h) {
		return nil
	}
	if s.isOwner(h) {
		return nil
	}
	effective, err := s.effectiveRights(h, folder)
	if err != nil {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Text: "ACL read failed: " + err.Error(),
		}
	}
	if effective.Has(right) {
		return nil
	}
	return s.aclRefusal(h, folder, right)
}

// requireMetadataAccess gates RFC 5464 mailbox METADATA: the accessing user
// needs the lookup right plus at least one access right (r/s/w/i/p). Owner
// and ACL-disabled sessions pass without a lookup.
func (s *session) requireMetadataAccess(h *nsHandle, folder string) error {
	if !s.aclEnforced(h) {
		return nil
	}
	if s.isOwner(h) {
		return nil
	}
	effective, err := s.effectiveRights(h, folder)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "ACL read failed: " + err.Error()}
	}
	access := effective.Has(mailbox.RightRead) || effective.Has(mailbox.RightWriteSeen) ||
		effective.Has(mailbox.RightWrite) || effective.Has(mailbox.RightInsert) ||
		effective.Has(mailbox.RightPost)
	if effective.Has(mailbox.RightLookup) && access {
		return nil
	}
	if !effective.Has(mailbox.RightLookup) {
		// Same answer an absent mailbox gets: without the lookup right the
		// user must not learn this one is there (#1068).
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Code: imaplib.ResponseCodeNoPerm,
		Text: "Permission denied: METADATA requires the 'l' right plus one of r/s/w/i/p",
	}
}

// requireAllRights requires every code in rights to be present. Used by
// STORE, which may carry mixed flag categories (\Seen, \Deleted, keywords)
// mapping to s/t/w.
func (s *session) requireAllRights(h *nsHandle, folder string, rights []rune) error {
	if !s.aclEnforced(h) {
		return nil
	}
	if s.isOwner(h) {
		return nil
	}
	if len(rights) == 0 {
		return nil
	}
	effective, err := s.effectiveRights(h, folder)
	if err != nil {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Text: "ACL read failed: " + err.Error(),
		}
	}
	for _, r := range rights {
		if !effective.Has(r) {
			return s.aclRefusal(h, folder, r)
		}
	}
	return nil
}

// requireRightOnParent enforces right on folder's parent. CREATE / RENAME
// (destination) require 'k' on the parent rather than on the not-yet-
// existing folder. A folder with no separator addresses the namespace-root
// ACL (<home>/yarilo-acl), where non-owners need an explicit grant to
// create top-level mailboxes.
func (s *session) requireRightOnParent(h *nsHandle, folder string, right rune) error {
	if !s.aclEnforced(h) {
		return nil
	}
	if s.isOwner(h) {
		return nil
	}
	parent := parentFolder(folder, byte(h.spec.Separator))
	effective, err := s.effectiveRights(h, parent)
	if err != nil {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Text: "ACL read failed: " + err.Error(),
		}
	}
	if effective.Has(right) {
		return nil
	}
	// Deliberately not the hidden-existence answer. CREATE names a mailbox
	// that does not exist yet, so "No such mailbox" would be true of the
	// request and tell the user nothing about what went wrong; the disclosure
	// this avoids elsewhere is about mailboxes that *are* there (#1068).
	return aclDenied(right)
}

// parentFolder returns folder with its last segment stripped, or
// "" when folder has no separator. Mirrors lastSepIndex in
// internal/userstate/acl.
func parentFolder(folder string, sep byte) string {
	if sep == 0 {
		return ""
	}
	for i := len(folder) - 1; i >= 0; i-- {
		if folder[i] == sep {
			return folder[:i]
		}
	}
	return ""
}

// requireRightOnSelected enforces right on the currently-SELECTed mailbox.
// Used by FETCH / SEARCH / STORE / EXPUNGE, which operate on state captured
// in s.folder + s.folderNS at SELECT time.
func (s *session) requireRightOnSelected(right rune) error {
	if s.folderNS == nil || s.folder == nil {
		// No SELECT in progress: defensive nil so the helper does not panic.
		return nil
	}
	return s.requireRight(s.folderNS, s.folder.Name, right)
}

// requireAllRightsOnSelected mirrors requireRightOnSelected for the
// STORE-style multi-right case.
func (s *session) requireAllRightsOnSelected(rights []rune) error {
	if s.folderNS == nil || s.folder == nil {
		return nil
	}
	return s.requireAllRights(s.folderNS, s.folder.Name, rights)
}

// aclDenied builds the canonical NO/NOPERM error for an ACL denial. The
// message names the missing right so operators can debug from the client
// transcript without reading the on-disk ACL file.
func aclDenied(right rune) error {
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Code: imaplib.ResponseCodeNoPerm,
		Text: "Permission denied: missing right '" + string(right) + "'",
	}
}

// aclRefusal is the answer for a user who may not do something to a mailbox.
//
// Without the lookup right it is the same answer an absent mailbox gets. That
// is what closes the existence oracle: the commands check existence before
// rights, so a user who could tell "no such mailbox" from "not allowed" could
// enumerate names in a shared namespace they may not see. Making the two
// answers identical costs nothing and does not require reordering the thirteen
// commands that ask the question -- what leaked was the difference between the
// replies, not the order in which they were reached (#1068).
//
// RFC 4314 §4 permits either, and the reference implementation resolves it the
// same way: acl_mailbox_fail_not_found reports "no permission" to a user with
// the lookup right and "mailbox not found" to one without.
//
// With the lookup right the user already knows the mailbox is there, so the
// precise refusal is not a disclosure and is far more useful.
func (s *session) aclRefusal(h *nsHandle, folder string, right rune) error {
	if !s.aclEnforced(h) || s.isOwner(h) {
		return aclDenied(right)
	}
	effective, err := s.effectiveRights(h, folder)
	if err == nil && !effective.Has(mailbox.RightLookup) {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	return aclDenied(right)
}

// storeFlagRights maps a STORE flag list onto the set of right codes
// the call must hold. The mapping follows RFC 4314 §5.1.1:
//
//	\Seen           → 's'  (RightWriteSeen)
//	\Deleted        → 't'  (RightDeleteMessage)
//	any other flag  → 'w'  (RightWrite)
//
// A STORE that touches multiple categories accumulates all the
// matching rights; requireAllRights then enforces them as a set.
func storeFlagRights(flags []imaplib.Flag) []rune {
	var seen, deleted, other bool
	for _, f := range flags {
		switch f {
		case imaplib.FlagSeen:
			seen = true
		case imaplib.FlagDeleted:
			deleted = true
		default:
			other = true
		}
	}
	var out []rune
	if seen {
		out = append(out, mailbox.RightWriteSeen)
	}
	if deleted {
		out = append(out, mailbox.RightDeleteMessage)
	}
	if other {
		out = append(out, mailbox.RightWrite)
	}
	return out
}
