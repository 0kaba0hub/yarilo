package mailbox

import (
	"fmt"
	"strings"
)

// ValidateFolderName refuses a mailbox name that would resolve outside its own
// folder.
//
// The maildir layout is what makes this load-bearing: INBOX *is* the mail root
// and subfolders are `.name` siblings inside it, so a name that contributes
// nothing to the path lands on the mailbox itself. An empty name resolved to
// the root, and Delete on it removed every message and the index with it — the
// mailbox was not emptied, it was recreated, with a new UIDVALIDITY telling
// every client to discard what it knew. A single "." resolved one level higher
// still and removed the user's home directory (#1063).
//
// The dbox layouts survived the same names because they put every folder under
// a subdirectory, which is why this looked driver-specific rather than like the
// missing check it is.
func ValidateFolderName(name, sep string) error {
	if name == "" {
		return fmt.Errorf("mailbox: empty folder name resolves to the mailbox root")
	}
	if sep == "" {
		sep = "/"
	}
	// Both separators are examined, not just the configured one: the name has
	// to be safe as a path on the way in, and the on-disk separator is not
	// always the IMAP one.
	for _, s := range []string{sep, "/", "."} {
		for _, segment := range strings.Split(name, s) {
			switch segment {
			case ".", "..":
				return fmt.Errorf("mailbox: folder name %q contains a %q path segment", name, segment)
			}
		}
	}
	if strings.Contains(name, "\x00") {
		return fmt.Errorf("mailbox: folder name contains a NUL")
	}
	return nil
}
