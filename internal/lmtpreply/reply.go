// Package lmtpreply holds helpers shared by the LMTP proxy implementations
// (internal/lmtp and internal/lmtplogin) so a fix to the relayed reply format
// lands in one place rather than diverging between the two proxies.
package lmtpreply

import (
	"strings"

	goSmtp "github.com/emersion/go-smtp"
)

// StripRcptPrefix removes a leading "<rcpt> " that the backend LMTP server
// prepends to each per-recipient DATA reply. A relaying login server prepends
// its own "<rcpt> ", so without stripping here the address appears twice
// (e.g. "452 4.2.2 <u@x> <u@x> Mailbox full"). Returns e unchanged when the
// prefix is absent.
func StripRcptPrefix(e *goSmtp.SMTPError, rcpt string) *goSmtp.SMTPError {
	prefix := "<" + rcpt + "> "
	if e == nil || !strings.HasPrefix(e.Message, prefix) {
		return e
	}
	clone := *e
	clone.Message = strings.TrimPrefix(e.Message, prefix)
	return &clone
}
