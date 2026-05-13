package lmtp

import "strings"

// lmtpWorkarounds is a bitmask of active client workarounds.
type lmtpWorkarounds uint32

const (
	// workaroundWhitespaceBeforePath allows whitespace before the path in
	// MAIL FROM and RCPT TO commands (e.g. "MAIL FROM: <user@example.com>").
	workaroundWhitespaceBeforePath lmtpWorkarounds = 1 << iota
	// workaroundMailboxForPath allows a bare mailbox name (no domain) in
	// RCPT TO (e.g. "RCPT TO:<alice>").
	workaroundMailboxForPath
)

// parseWorkarounds parses a space/comma-separated list of workaround names
// into a bitmask. Unknown names are silently ignored (matching Dovecot behaviour).
func parseWorkarounds(list []string) lmtpWorkarounds {
	var mask lmtpWorkarounds
	for _, item := range list {
		switch strings.ToLower(strings.TrimSpace(item)) {
		case "whitespace-before-path":
			mask |= workaroundWhitespaceBeforePath
		case "mailbox-for-path":
			mask |= workaroundMailboxForPath
		}
	}
	return mask
}
