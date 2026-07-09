package mailbox

import (
	"path/filepath"
	"strings"
)

// FolderSubpath returns the per-folder directory for a driver's on-disk
// layout, relative to a storage root. Rooting the result at the mail root
// yields the mailbox path; rooting it at the index root yields the index
// path — the same sub-layout under either root. This is the single builder
// shared by the mailbox backends, the fileindex and the ACL store, so those
// trees share their folder layout by construction instead of drifting between
// independent implementations.
//
// diskName is the already-encoded folder name (callers apply NFC/modUTF7);
// INBOX is passed through unchanged. folder is the logical name, used only
// to special-case INBOX. sep is the IMAP hierarchy separator embedded in the
// name; each driver converts it to its own on-disk separator:
//
//	maildir      : flat, "."-joined  → INBOX -> "",  A<sep>B -> ".A.B"
//	mdbox/sdbox  : nested, "/"-joined → "mailboxes/A/B/dbox-Mails"
//
// For the dbox drivers the per-folder dbox-Mails directory is the mailbox
// marker: its presence makes a directory selectable, its absence marks a
// \NoSelect parent container. mdbox and sdbox share the layout; they differ
// only in what the marker dir holds (sdbox: message files; mdbox: nothing —
// payloads live in the shared storage/, the index is external).
//
// For maildir, INBOX IS the maildir root (no INBOX/ subdir). An empty sep
// defaults to "/" so callers that never set one keep the historical layout.
func FolderSubpath(driver, folder, diskName, sep string) string {
	if sep == "" {
		sep = "/"
	}
	switch driver {
	case "mdbox", "sdbox", "dbox":
		return filepath.Join(mailboxesSubdir, toDiskSep(diskName, sep, "/"), dboxMailsSubdir)
	default: // maildir — maildir++ flat layout, "." separates hierarchy
		if folder == "INBOX" {
			return ""
		}
		return "." + toDiskSep(diskName, sep, ".")
	}
}

// SepOrDefault returns the IMAP hierarchy separator, defaulting to "/" when
// unset. Backends store the result so reverse mapping (on-disk → IMAP) never
// sees an empty separator.
func SepOrDefault(s string) string {
	if s == "" {
		return "/"
	}
	return s
}

// toDiskSep rewrites the IMAP hierarchy separator in name to the driver's
// on-disk separator. When they already match it is a no-op.
func toDiskSep(name, imapSep, diskSep string) string {
	if imapSep == diskSep {
		return name
	}
	return strings.ReplaceAll(name, imapSep, diskSep)
}

const (
	mailboxesSubdir = "mailboxes"
	dboxMailsSubdir = "dbox-Mails"
)
