package mailbox

// StampLocation parses mailLoc and stamps the per-user driver and the
// INDEX=/CONTROL=/ALT=/VOLATILEDIR= modifiers onto ui. Separate userdb dir
// fields already set on ui win — an embedded modifier only fills a blank. A
// leading "~/" in a modifier expands to the user's home. It returns an error
// (leaving ui unstamped) only for a genuinely unknown or malformed
// mail_location, so the caller can warn with its own prefix and keep the user on
// the global backend; a recognised driver whose path is empty (derived from
// home, e.g. "mdbox:") is a valid form and its driver is still stamped. Empty
// mailLoc is a no-op.
func StampLocation(ui *UserInfo, mailLoc string) error {
	if mailLoc == "" {
		return nil
	}
	loc, ok, err := ParseLocation(mailLoc, ui)
	if err != nil {
		// A recognised driver with an empty path is valid (path comes from the
		// home template); stamp just the driver. Everything else is a real error.
		if d := LocationDriver(mailLoc); d != "" {
			ui.Driver = d
			return nil
		}
		return err
	}
	if !ok {
		return nil
	}
	ui.Driver = loc.Driver
	// ParseLocation expands %u/%n/%d/%h but not a leading "~/"; ExpandHome does,
	// and is a no-op on absolute/empty paths. Separate userdb fields win.
	for _, m := range []struct {
		dst *string
		src string
	}{
		{&ui.IndexDir, loc.IndexDir},
		{&ui.VolatileDir, loc.VolatileDir},
		{&ui.ControlDir, loc.ControlDir},
		{&ui.AltDir, loc.AltDir},
	} {
		if *m.dst == "" {
			*m.dst = ExpandHome(m.src, ui.Home)
		}
	}
	return nil
}

// SelectPersonalBackend is the single gating rule for picking a user's personal
// mailbox backend: the per-user backend from byDriver when a driver is set and
// the factory returns one, otherwise global. An empty driver (no per-user
// mail_location, or an unrecognised one StampLocation refused to stamp) falls
// through to global.
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
