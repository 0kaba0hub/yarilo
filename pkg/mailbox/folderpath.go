package mailbox

import "path/filepath"

// FolderSubpath returns the per-folder directory for a driver's on-disk
// layout, relative to a storage root. Rooting the result at the mail root
// yields the mailbox path; rooting it at the index root yields the index
// path — the same sub-layout under either root. This mirrors Dovecot's
// single fs_list_get_path_to builder, so the mailbox and index trees share
// their folder layout by construction instead of drifting between two
// independent implementations.
//
// diskName is the already-encoded folder name (callers apply NFC/modUTF7);
// INBOX is passed through unchanged. folder is the logical name, used only
// to special-case INBOX.
//
// Layouts:
//
//	maildir : INBOX -> "INBOX",  other -> ".<disk>"
//	mdbox   : "mailboxes/<disk>"
//	sdbox   : "mailboxes/<disk>/dbox-Mails"
func FolderSubpath(driver, folder, diskName string) string {
	switch driver {
	case "mdbox":
		return filepath.Join(mailboxesSubdir, diskName)
	case "sdbox", "dbox":
		return filepath.Join(mailboxesSubdir, diskName, dboxMailsSubdir)
	default: // maildir
		if folder == "INBOX" {
			return "INBOX"
		}
		return "." + diskName
	}
}

const (
	mailboxesSubdir = "mailboxes"
	dboxMailsSubdir = "dbox-Mails"
)
