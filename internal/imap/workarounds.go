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

// ParseIMAPWorkarounds converts a list of workaround names into a bitmask.
// Unknown names are silently ignored.
func ParseIMAPWorkarounds(list []string) imapWorkarounds {
	var mask imapWorkarounds
	for _, item := range list {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "tb-extra-mailbox-sep":
			mask |= workaroundTBExtraMailboxSep
		case "tb-lsub-flags":
			mask |= workaroundTBLSUBFlags
		}
	}
	return mask
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
