package mailbox

import "fmt"

// NamespaceUserInfo builds the UserInfo a non-personal namespace's storage
// handles resolve against: the session's identity, with every path field taken
// from the namespace's own location.
//
// It exists because two callers built this structure from one ParseLocation and
// differed by five fields. The IMAP path set all of them; the admin API set
// Username and Home. Those two are not equivalent -- with MailPath unset the
// consumers fall into different defaults, a maildir backend to Home/Maildir and
// the ACL store to Home -- so the admin API looked for a mailbox in
// <root>/Maildir/.News while its ACL file sat in <root>/, and every per-mailbox
// admin ACL call on a shared namespace answered "folder not found" for a
// mailbox that was plainly there (#1109).
//
// Driver matters for the same reason one step further out: unset, the bundle
// falls back to the deployment-wide driver rather than the one the namespace's
// location names, which is correct only while every namespace uses one format.
//
// separator comes from the namespace spec rather than from base: it is a
// property of the namespace, and the personal one may use another.
//
// An empty loc.Path is refused rather than defaulted. Every field here exists
// because a consumer left to its own default resolves somewhere else, so a
// constructor that accepted an empty root would reintroduce the class it is
// meant to close -- and it would do so from the one place that is supposed to
// make it impossible. The reference asserts at the same point
// (mailbox-list.c:132).
func NamespaceUserInfo(base *UserInfo, loc Location, separator string) (*UserInfo, error) {
	if loc.Path == "" {
		return nil, fmt.Errorf("mailbox: namespace location has no root path")
	}
	ui := &UserInfo{
		Home:        loc.Path,
		MailPath:    loc.Path,
		Driver:      loc.Driver,
		IndexDir:    loc.IndexDir,
		VolatileDir: loc.VolatileDir,
		ControlDir:  loc.ControlDir,
		AltDir:      loc.AltDir,
		Separator:   separator,
	}
	if base != nil {
		ui.Username = base.Username
		// Storage-name form is a deployment-wide property, not a per-namespace
		// one: a namespace that escaped or normalised differently from the rest
		// would name the same mailbox differently on disk (#1078, #1092).
		ui.StorageEscapeChar = base.StorageEscapeChar
		ui.SkipNFCNormalize = base.SkipNFCNormalize
		// The ACL identity travels too. Leaving it out is what made this
		// constructor not shared: the delivery path carried these and the other
		// two did not, so an ACL entry naming a group resolved differently at
		// delivery than at SELECT of the same shared mailbox -- and a later
		// change routing delivery through here would have silently switched
		// group ACLs off instead.
		ui.Groups = base.Groups
		ui.ACLUser = base.ACLUser
		ui.ACLGroups = base.ACLGroups
	}
	return ui, nil
}
