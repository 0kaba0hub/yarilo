package mailbox

import "log/slog"

// StampLocation parses mailLoc and stamps the per-user driver and the
// INDEX=/CONTROL=/ALT=/VOLATILEDIR= modifiers onto ui. Separate userdb dir
// fields already set on ui win — an embedded modifier only fills a blank
// (Dovecot / IMAP / LMTP priority). A malformed or unknown-driver mail_location
// logs a warning and leaves ui unchanged (no driver stamp), so a driver typo
// cannot silently select a mailbox backend that mismatches the index layout the
// driver would pick. Empty mailLoc is a no-op.
func StampLocation(ui *UserInfo, mailLoc string) {
	if mailLoc == "" {
		return
	}
	loc, ok, err := ParseLocation(mailLoc, ui)
	if err != nil {
		slog.Warn("mailbox: mail_location parse failed; ignoring per-user driver",
			"user", ui.Username, "mail_location", mailLoc, "err", err)
		return
	}
	if !ok {
		return
	}
	ui.Driver = loc.Driver
	// ParseLocation expands %u/%n/%d/%h but not a leading "~/"; do that here so
	// a "~/index" modifier resolves to the user's home (the IMAP inline parser
	// did this via ExpandHome, and POP3/LMTP now match). ExpandHome is a no-op on
	// absolute or empty paths.
	if ui.IndexDir == "" {
		ui.IndexDir = ExpandHome(loc.IndexDir, ui.Home)
	}
	if ui.VolatileDir == "" {
		ui.VolatileDir = ExpandHome(loc.VolatileDir, ui.Home)
	}
	if ui.ControlDir == "" {
		ui.ControlDir = ExpandHome(loc.ControlDir, ui.Home)
	}
	if ui.AltDir == "" {
		ui.AltDir = ExpandHome(loc.AltDir, ui.Home)
	}
}

// SelectPersonalBackend is the single gating rule for picking a user's personal
// mailbox backend: the per-user backend from byDriver when a driver is set and
// the factory returns one, otherwise the global backend. byDriver never
// returning nil for a recognised driver means a stamped driver always resolves;
// an empty driver (no per-user mail_location, or an unrecognised one that
// StampLocation refused to stamp) falls through to global.
func SelectPersonalBackend(global MailboxBackend, byDriver func(string) MailboxBackend, driver string) MailboxBackend {
	if driver == "" {
		return global
	}
	if byDriver != nil {
		if b := byDriver(driver); b != nil {
			return b
		}
	}
	return global
}

// ResolvePersonalStorage stamps ui from mailLoc and returns the personal
// mailbox backend in one call — the shared resolver IMAP, POP3 and LMTP use so
// every protocol reads the same store and per-folder index layout for a user.
func ResolvePersonalStorage(global MailboxBackend, byDriver func(string) MailboxBackend, mailLoc string, ui *UserInfo) MailboxBackend {
	StampLocation(ui, mailLoc)
	return SelectPersonalBackend(global, byDriver, ui.Driver)
}
