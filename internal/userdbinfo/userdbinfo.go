// Package userdbinfo overlays a userdb lookup onto a resolver-derived UserInfo.
// Per-user overrides (INDEX=, CONTROL=, ALT=, mail_path) live in the userdb, so
// anything addressing real storage applies them the same way sessions do.
package userdbinfo

import (
	"strings"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Apply overlays pui onto ui. Dedicated fields win; a userdb driver that bakes
// everything into one mail_location string is parsed for the same modifiers.
func Apply(ui *mailbox.UserInfo, pui *protocol.UserInfo, username string) {
	if ui == nil || pui == nil {
		return
	}
	expand := func(v string) string {
		v = mailbox.ExpandHome(v, ui.Home)
		return mailbox.ExpandVars(strings.ReplaceAll(v, "%h", ui.Home), username)
	}
	if pui.VolatileDir != "" {
		ui.VolatileDir = expand(pui.VolatileDir)
	}
	if pui.IndexDir != "" {
		ui.IndexDir = expand(pui.IndexDir)
	}
	if pui.ControlDir != "" {
		ui.ControlDir = expand(pui.ControlDir)
	}
	if pui.AltDir != "" {
		ui.AltDir = expand(pui.AltDir)
	}
	if pui.MailPath != "" {
		ui.MailPath = mailbox.ExpandHome(pui.MailPath, ui.Home)
	} else if pui.MailLocation != "" {
		// mail_location = "driver:path[:MODS]": take the path when userdb
		// returned no explicit mail_path field.
		if colon := strings.IndexByte(pui.MailLocation, ':'); colon >= 0 {
			rest := pui.MailLocation[colon+1:]
			if nextColon := strings.IndexByte(rest, ':'); nextColon >= 0 {
				rest = rest[:nextColon]
			}
			if rest != "" {
				ui.MailPath = expand(rest)
			}
		}
	}
	if pui.MailLocation != "" {
		applyMailLocationMods(pui.MailLocation, ui, username)
		if colon := strings.IndexByte(pui.MailLocation, ':'); colon > 0 {
			ui.Driver = strings.ToLower(pui.MailLocation[:colon])
		}
	}
	if pui.InboxPath != "" {
		ui.InboxPath = mailbox.ExpandHome(pui.InboxPath, ui.Home)
	}
}

// applyMailLocationMods reads the "driver:path:KEY=value" modifiers. A value
// already set from a dedicated field wins, so the modifiers only fill gaps.
func applyMailLocationMods(loc string, ui *mailbox.UserInfo, username string) {
	expand := func(v string) string {
		v = mailbox.ExpandHome(v, ui.Home)
		return mailbox.ExpandVars(strings.ReplaceAll(v, "%h", ui.Home), username)
	}
	colon := strings.IndexByte(loc, ':')
	if colon < 0 {
		return
	}
	rest := loc[colon+1:]
	colon = strings.IndexByte(rest, ':')
	if colon < 0 {
		return
	}
	for _, mod := range strings.Split(rest[colon+1:], ":") {
		key, val, ok := strings.Cut(mod, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(key) {
		case "INDEX":
			if ui.IndexDir == "" {
				ui.IndexDir = expand(val)
			}
		case "VOLATILEDIR":
			if ui.VolatileDir == "" {
				ui.VolatileDir = expand(val)
			}
		case "CONTROL":
			if ui.ControlDir == "" {
				ui.ControlDir = expand(val)
			}
		case "ALT":
			if ui.AltDir == "" {
				ui.AltDir = expand(val)
			}
		}
	}
}
