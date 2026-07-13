package sieve

import (
	"strings"

	"github.com/foxcpp/go-sieve/interp"
)

// yariloEnv implements interp.Env for the vnd.yarilo.environment extension.
//
// Exposed items:
//   - vnd.yarilo.username        — full login name (user@domain)
//   - vnd.yarilo.default-mailbox — always "INBOX"
//   - vnd.yarilo.config.<key>    — operator-defined key-value pairs from
//     yarilo.yaml sieve.sieve_environment
type yariloEnv struct {
	username    string
	configItems map[string]string
}

var _ interp.Env = (*yariloEnv)(nil)

func (e *yariloEnv) GetEnvironment(name string) (string, bool) {
	switch name {
	case "vnd.yarilo.username":
		return e.username, true
	case "vnd.yarilo.default-mailbox":
		return "INBOX", true
	}
	if key, ok := strings.CutPrefix(name, "vnd.yarilo.config."); ok {
		v, found := e.configItems[key]
		return v, found
	}
	return "", false
}

// imapEnv wraps yariloEnv with the RFC 6785 imap.* items available to imapsieve
// scripts triggered by an IMAP event (APPEND / COPY / FLAG).
type imapEnv struct {
	base         *yariloEnv
	cause        string // "APPEND", "COPY", or "FLAG"
	mailbox      string // affected (destination) mailbox
	email        string // script owner, user@domain
	changedFlags string // space-separated flags (FLAG cause)
	fromMailbox  string // COPY/MOVE source mailbox
	toMailbox    string // COPY/MOVE destination mailbox
}

var _ interp.Env = (*imapEnv)(nil)

func (e *imapEnv) GetEnvironment(name string) (string, bool) {
	switch name {
	case interp.EnvImapCause:
		return e.cause, e.cause != ""
	case interp.EnvImapMailbox:
		return e.mailbox, e.mailbox != ""
	case interp.EnvImapEmail:
		return e.email, e.email != ""
	case interp.EnvImapUser:
		return e.base.username, e.base.username != ""
	case interp.EnvImapChangedFlags:
		return e.changedFlags, true
	case interp.EnvVndMailboxFrom:
		return e.fromMailbox, e.fromMailbox != ""
	case interp.EnvVndMailboxTo:
		return e.toMailbox, e.toMailbox != ""
	}
	return e.base.GetEnvironment(name)
}
