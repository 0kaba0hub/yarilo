package imap

import (
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
)

type imapWorkarounds uint32

const (
	// workaroundTBExtraMailboxSep strips a leading hierarchy separator from
	// LIST ref and patterns (Thunderbird bug: sends "/Sent" instead of "Sent").
	workaroundTBExtraMailboxSep imapWorkarounds = 1 << iota
	// workaroundTBLSUBFlags adds \NoInferiors to leaf mailboxes in LIST
	// responses so Thunderbird knows the folder has no children.
	workaroundTBLSUBFlags
)

// ParseIMAPWorkarounds converts a list of workaround names into a bitmask, and
// returns the names it did not recognise.
//
// The unknown ones are returned rather than dropped: a misspelled workaround
// used to be accepted and do nothing, so an operator who set it saw the
// behaviour they were working around continue, with the configuration that was
// supposed to fix it sitting right there. Silence is the worst answer a
// configuration parser can give.
func ParseIMAPWorkarounds(list []string) (imapWorkarounds, []string) {
	var mask imapWorkarounds
	var unknown []string
	for _, item := range list {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "tb-extra-mailbox-sep":
			mask |= workaroundTBExtraMailboxSep
		case "tb-lsub-flags":
			mask |= workaroundTBLSUBFlags
		case "":
		default:
			unknown = append(unknown, item)
		}
	}
	return mask, unknown
}

// isLeaf reports whether name has no children in folders.
func isLeaf(name string, folders []string, sep string) bool {
	prefix := name + sep
	for _, f := range folders {
		if strings.HasPrefix(f, prefix) {
			return false
		}
	}
	return true
}

// mailboxAttrs returns the LIST attributes for a folder given active
// workarounds. sep is the namespace separator embedded in folder names.
func mailboxAttrs(name string, folders []string, sep string, w imapWorkarounds) []imaplib.MailboxAttr {
	if w&workaroundTBLSUBFlags != 0 && isLeaf(name, folders, sep) {
		return []imaplib.MailboxAttr{imaplib.MailboxAttrNoInferiors}
	}
	return nil
}

// KnownWorkarounds is the accepted set, so a warning about an unknown name can
// print what the operator could have meant.
func KnownWorkarounds() []string {
	return []string{"tb-extra-mailbox-sep", "tb-lsub-flags"}
}
