package imap

import (
	imaplib "github.com/emersion/go-imap/v2"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// ACL enforcement entry points. The require* helpers below resolve a
// folder name to its namespace handle, load the stored ACL, compute
// the accessing user's effective rights (PR D scope — no inheritance,
// no group resolution, see pkg/mailbox.ACL.Effective), and return a
// NO error with RFC 5530 NOPERM code when the requested right is
// absent.
//
// Out of PR D scope, deferred to PR E:
//   - Walking ancestors when the mailbox has no explicit yarilo-acl
//     file (currently treated as "no rights" for non-owners).
//   - Group / group-override identifier resolution.
//   - Owner identifier auto-grant beyond personal-namespace ownership.
//
// When ACLEnabled is false, every require* helper returns nil (the
// extension is off — no enforcement, no NOPERM). Operators can still
// configure ACL files on disk; they just have no runtime effect until
// the feature is enabled.

// isOwner reports whether the accessing session owns the mailbox in
// the given namespace handle. PR D: owner == personal namespace —
// every mailbox in the user's own home is owned by them, regardless
// of any explicit `owner` ACL entry. Shared / public mailboxes are
// never auto-owned in PR D scope.
func (s *session) isOwner(h *nsHandle) bool {
	return h.spec.Type == NamespacePersonal
}

// insertRight returns the right code APPEND / COPY destination /
// MOVE destination must carry, picking by namespace type (RFC 4314
// §5.1.1): personal → 'i' (insert), shared / public → 'p' (post).
//
// Public namespaces typically receive mail via MTA rather than IMAP
// APPEND, but the right code still differs per RFC 4314 §5.1.1.
func insertRight(spec NamespaceSpec) rune {
	if spec.Type == NamespacePersonal {
		return mailbox.RightInsert
	}
	return mailbox.RightPost
}

// effectiveRights resolves the accessing user's effective rights on
// folder under h, walking ancestors when no explicit ACL is present
// (first-ancestor-with-explicit-ACL walk — see
// internal/userstate/acl.Store.EffectiveFor). The namespace's
// hierarchy separator drives the walk; sep == 0 disables it.
func (s *session) effectiveRights(h *nsHandle, folder string) (mailbox.Rights, error) {
	return h.acl.EffectiveFor(folder, s.userInfo.Username, s.userInfo.Groups, s.isOwner(h), byte(h.spec.Separator))
}

// requireRight loads the effective ACL for folder under h (with
// inheritance walk through ancestors) and returns nil when the
// accessing user holds right, or a NO/NOPERM error otherwise.
// Errors from the ACL store (parse / I/O) surface as NO with the
// underlying message — they are not silently treated as "denied" so
// operators can debug from the client transcript.
func (s *session) requireRight(h *nsHandle, folder string, right rune) error {
	if !s.srv.opts.ACLEnabled {
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
	return aclDenied(right)
}

// requireAllRights folds a set of right codes into a single check —
// every code must be present. Used by STORE which may carry mixed
// flag categories (\Seen, \Deleted, plus arbitrary keywords mapping
// to s + t + w respectively).
func (s *session) requireAllRights(h *nsHandle, folder string, rights []rune) error {
	if !s.srv.opts.ACLEnabled {
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
			return aclDenied(r)
		}
	}
	return nil
}

// requireRightOnParent enforces a right on the parent of folder —
// CREATE / RENAME (destination side) require 'k' on the parent
// mailbox rather than on the folder itself (which does not yet
// exist). The parent is computed by stripping the trailing segment
// after the namespace separator; if folder has no separator, the
// parent is the empty string addressing the namespace-root ACL
// (<home>/yarilo-acl) — non-owners need an explicit grant there to
// create top-level mailboxes.
func (s *session) requireRightOnParent(h *nsHandle, folder string, right rune) error {
	if !s.srv.opts.ACLEnabled {
		return nil
	}
	if s.isOwner(h) {
		return nil
	}
	parent := parentFolder(folder, byte(h.spec.Separator))
	return s.requireRight(h, parent, right)
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

// requireRightOnSelected enforces right on the currently-SELECTed
// mailbox. Used by FETCH / SEARCH / STORE / EXPUNGE which operate on
// state captured in s.folder + s.folderNS at SELECT time.
func (s *session) requireRightOnSelected(right rune) error {
	if s.folderNS == nil || s.folder == nil {
		// No SELECT in progress — the lib will reject the command
		// for state reasons before reaching us, but keep a defensive
		// nil here so the helper does not panic.
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

// aclDenied builds the canonical NO/NOPERM error for an ACL denial.
// The message names the missing right so operators can debug from
// the client transcript without having to read the on-disk ACL file.
func aclDenied(right rune) error {
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Code: imaplib.ResponseCodeNoPerm,
		Text: "Permission denied: missing right '" + string(right) + "'",
	}
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
