package imap

import (
	"errors"
	"fmt"
	"log/slog"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/locks"
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
// isOwner reports whether the session user owns this instance of the namespace.
//
// By person, not by namespace type: h.owner is the principal who owns the
// instance ("" when none), and this compares it against the session user. For a
// personal namespace the two are the same user, so the answer is unchanged from
// the old type-based predicate; for a fixed shared/public namespace h.owner is
// "" and nobody is the owner. The person-based form is what an owner-templated
// namespace (B1) needs -- and it is the definition adminCheckPRc once carried
// privately before it was removed so there would be exactly one (#1107, #1119
// removed insertRight for the same reason: a predicate deciding by type where
// the question is about the subject).
func (s *session) isOwner(h *nsHandle) bool {
	return h.owner != "" && s.userInfo != nil && s.userInfo.Username == h.owner
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
	effective, err := s.effectiveRights(h, folder)
	if err != nil {
		return aclUnavailable(folder, err)
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
	effective, err := s.effectiveRights(h, folder)
	if err != nil {
		return aclUnavailable(folder, err)
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
	if len(rights) == 0 {
		return nil
	}
	effective, err := s.effectiveRights(h, folder)
	if err != nil {
		return aclUnavailable(folder, err)
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
	parent := parentFolder(folder, byte(h.spec.Separator))
	effective, err := s.effectiveRights(h, parent)
	if err != nil {
		return aclUnavailable(folder, err)
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

// grantCreatorAdmin gives the creator of a mailbox the admin right on it, in a
// namespace nobody owns.
//
// Without it such a mailbox can end up with no administrator at all. A personal
// or owner-templated namespace always has one -- the owner resolves to
// FullRights by construction -- but a fixed shared/public namespace is owned by
// nobody on purpose, so the only source of 'a' is whatever the namespace root
// happened to grant. When the root grant omits it, the mailbox that was just
// created cannot be administered by its creator, by an operator, or by anyone,
// and no ACL command can repair that, because repairing it needs the right that
// is missing. RFC 4314 §4 does not allow 'a' to become unobtainable (#1320).
//
// It adds 'a' ON TOP of what was inherited rather than granting full rights:
// restrictions expressed by leaving a right out of the root ACL keep holding,
// and CREATE does not become a way around them. The trade is the plain reading
// of the create right -- letting someone create a mailbox here means letting
// them administer what they created.
//
// A creator who already holds 'a' by inheritance is left alone, so this writes
// nothing in the ordinary case.
func (s *session) grantCreatorAdmin(h *nsHandle, folder string) error {
	if h.owner != "" || s.userInfo == nil {
		return nil
	}
	effective, err := s.effectiveRights(h, folder)
	if err != nil {
		return err
	}
	if effective.Has(mailbox.RightAdminister) {
		return nil
	}
	aclUser, _ := s.userInfo.ACLIdentity()
	if err := mailbox.ValidIdentifier(aclUser); err != nil {
		return err
	}
	me := mailbox.Identifier{Type: mailbox.IDUser, Name: aclUser}
	return h.acl.Update(folder, func(cur mailbox.ACL) (mailbox.ACL, error) {
		for i, e := range cur {
			if e.Negative || e.Identifier != me {
				continue
			}
			cur[i].Rights = e.Rights.Add(mailbox.Rights(string(mailbox.RightAdminister)))
			return cur, nil
		}
		return append(cur, mailbox.Entry{
			Identifier: me,
			Rights:     effective.Add(mailbox.Rights(string(mailbox.RightAdminister))),
		}), nil
	})
}

// rollBackUnadministered undoes a CREATE whose creator ended up with no admin
// right, and reports the CREATE as failed.
//
// The two steps after CREATE fail into different states, which is why only this
// one is undone. A failed MaterialiseOnCreate leaves a mailbox governed by the
// namespace root, exactly as before that feature existed. A failed grant can
// leave a mailbox nobody can administer -- and it is sticky: the next CREATE of
// the same name fails with "already exists", so nothing self-heals, and
// repairing it needs the very right that is missing (#1334).
//
// The window is not theoretical. CREATE is a local mkdir; the ACL write goes
// through yarilo-locks -- a separate deployment, over the network, with a
// 30-second acquisition timeout. A rolling upgrade of that deployment is tens
// of seconds during which the two can disagree.
//
// The decision rests on LIVE effective rights, not on what was written -- but
// it is grantCreatorAdmin that reads them, and this runs only when that read
// said there is no 'a' or could not be made at all. A mailbox the namespace
// root administers by inheritance therefore never reaches here: the grant
// returns early and nothing is undone.
//
// There is deliberately no second read to confirm. Reads take the same lock the
// write does, so in the failure this exists for -- the lock service being
// unreachable -- a confirming read cannot succeed either. Requiring one would
// mean the rollback never fires exactly when it is needed, and treating an
// unreadable ACL store as "probably fine" is how the mailbox is left
// unadministered.
func (s *session) rollBackUnadministered(h *nsHandle, folder, wireName string, cause error) error {
	aclUser, _ := s.userInfo.ACLIdentity()
	if err := h.box.Delete(folder); err != nil {
		// The mailbox exists, nobody can administer it, and the undo failed
		// too. Nothing here can fix that, so it is said once, loudly, with the
		// command that does -- run by someone who still holds 'a' above it.
		slog.Error("imap: mailbox left with no administrator; delete it or grant the right",
			"folder", wireName, "identifier", aclUser, "grant_err", cause, "delete_err", err,
			"repair", fmt.Sprintf("SETACL %q user=%s a", wireName, aclUser))
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Text: "mailbox created but could not be granted an administrator, and could not be removed: " + cause.Error(),
		}
	}
	if err := h.idx.DeleteFolder(folder); err != nil {
		slog.Warn("imap: index state left behind by a rolled-back CREATE", "folder", wireName, "err", err)
	}
	slog.Warn("imap: CREATE rolled back, the creator could not be granted the admin right",
		"folder", wireName, "identifier", aclUser, "err", cause)
	// Reported as failed, not as OK with a warning nobody reads: the client
	// must see that the mailbox is not there, so that retrying is the obvious
	// next move once the ACL store answers again.
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Text: "could not grant an administrator on the new mailbox; it was removed, try again",
	}
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
	// The owner never reaches here: they resolve to FullRights, so no required
	// right is ever absent for them. That is by construction, not an early exit
	// in this function -- if a resolver change ever let the owner miss a right,
	// this hiding path must not start returning "No such mailbox" for the
	// owner's own folder, which a test pins (TestOwnerNeverGetsHiddenExistence).
	if !s.aclEnforced(h) {
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

// aclUnavailable is the answer to a client when the ACL state cannot be read.
//
// The error carries our plumbing: package paths, the lock key -- which contains
// the account name -- and the transport detail underneath. None of it means
// anything to a mail client, and a client is not the place to publish the shape
// of our internals (#1341). The text a client sees is therefore stable and
// says what to do; the cause goes to the log, where it is useful and where it
// already has the folder and the session beside it.
func aclUnavailable(folder string, err error) error {
	slog.Warn("imap: acl state unavailable", "folder", folder, "err", err)
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Code: unavailableOr(err, imaplib.ResponseCodeServerBug),
		Text: "Mailbox permissions are temporarily unavailable, try again",
	}
}

// unavailableOr classifies a failure for the client: UNAVAILABLE when a
// dependency is merely absent, otherwise the caller's fallback.
//
// The distinction is not cosmetic. SERVERBUG means "this server is broken, the
// request will never work", and a client that receives it stops retrying and
// shows the user something they cannot act on; UNAVAILABLE (RFC 5530) means
// "temporarily, try again", which is what a redeployed lock service actually
// is -- the same request succeeds seconds later (#1339).
func unavailableOr(err error, fallback imaplib.ResponseCode) imaplib.ResponseCode {
	if errors.Is(err, locks.ErrUnavailable) {
		return imaplib.ResponseCodeUnavailable
	}
	return fallback
}

// dependencyError re-classifies a storage error on its way to the client.
//
// Anything that is not an *imap.Error is turned into NO [SERVERBUG] by the
// library, so a lock service being restarted reached clients as "this server is
// broken" -- and the operator went looking for a crash that never happened.
// Errors that are not a dependency outage are returned untouched, so nothing
// else is reclassified by accident.
func dependencyError(err error) error {
	if err == nil || !errors.Is(err, locks.ErrUnavailable) {
		return err
	}
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Code: imaplib.ResponseCodeUnavailable,
		Text: "Temporarily unavailable, try again",
	}
}
