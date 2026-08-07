package mailbox

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
func NamespaceUserInfo(base *UserInfo, loc Location, separator string) *UserInfo {
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
	}
	return ui
}
