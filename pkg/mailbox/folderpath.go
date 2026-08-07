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
	return FolderSubpathEscaped(driver, folder, diskName, sep, "")
}

// FolderSubpathEscaped is FolderSubpath with storage-name escaping applied.
//
// escape is a single character, or "" for the default of no escaping. When set,
// a layout separator the client wrote *literally* is hex-escaped, while the
// hierarchy the client expressed with the namespace separator still becomes
// levels on disk. That difference is the whole point: without it, "a.b" and
// "a/b" are the same bytes under a maildir layout and one of the two names is
// silently the other (#1078).
//
// It does NOT normalise the name. NFC is applied once, at the name-entry
// boundary (mailbox.NormalizeName), so by the time a name reaches here every
// tree already spells it the same way. This function used to normalise, the
// drivers also normalised, and the order between NFC and escaping was then held
// by convention across two owners -- which is the fault that kept coming back
// (#1078, #1092, #1113).
func FolderSubpathEscaped(driver, folder, diskName, sep, escape string) string {
	if sep == "" {
		sep = "/"
	}
	switch driver {
	case "mdbox", "sdbox", "dbox":
		return filepath.Join(mailboxesSubdir, toDiskSep(diskName, sep, "/", escape), dboxMailsSubdir)
	default: // maildir — maildir++ flat layout, "." separates hierarchy
		if folder == "INBOX" {
			return ""
		}
		return "." + toDiskSep(diskName, sep, ".", escape)
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
func toDiskSep(name, imapSep, diskSep, escape string) string {
	if escape == "" {
		if imapSep == diskSep {
			return name
		}
		return strings.ReplaceAll(name, imapSep, diskSep)
	}
	// Split on what the client meant as hierarchy first, escape each level,
	// then join with what the layout writes. Escaping after the join would
	// escape the separators the client asked for; escaping before the split
	// would hide them from it.
	parts := strings.Split(name, imapSep)
	for i, p := range parts {
		parts[i] = EscapeStorageName(p, diskSep, escape)
	}
	return strings.Join(parts, diskSep)
}

const (
	mailboxesSubdir = "mailboxes"
	dboxMailsSubdir = "dbox-Mails"
)
