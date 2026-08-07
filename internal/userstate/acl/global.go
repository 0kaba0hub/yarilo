package acl

import (
	"fmt"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Policy carries the operator ACL knobs a Store evaluates beyond the raw
// per-mailbox files: root-default-from-INBOX, global ACL rules, and the
// globals-only switch. Grouped so New stays stable as more knobs land.
type Policy struct {
	// DefaultsFromInbox resolves the namespace-root default from INBOX's ACL.
	DefaultsFromInbox bool
	// GlobalsOnly ignores the per-mailbox files and evaluates only Global.
	GlobalsOnly bool
	// Global is the operator-configured global ACL, or nil when none.
	Global *Global
	// CacheTTL bounds how long a parsed per-mailbox ACL is trusted before the
	// file's mtime+size are re-validated (the acl_cache_ttl knob, default 30s).
	// Zero disables caching — every read hits the filesystem.
	CacheTTL time.Duration
	// SkipNFCNormalize mirrors UserInfo.SkipNFCNormalize: the ACL file has to
	// land beside the folder the mail driver made, so it derives its path in
	// the same name form (#1092). Inverted for the same reason -- the zero
	// value must mean "normalise", since the config key defaults to true.
	SkipNFCNormalize bool
}

// Global is the parsed, operator-configured global ACL: rules that apply
// across all users and merge with per-mailbox ACLs (global takes precedence).
// A nil *Global means "no global ACL".
type Global struct {
	rules []globalRule
}

type globalRule struct {
	mailbox string // exact mailbox name, or "*" for every mailbox
	acl     mailbox.ACL
}

// NewGlobal parses config rules into a Global. Returns (nil, nil) when there
// are no rules so callers can treat the absence of a global ACL as a nil
// *Global. Errors on an unparseable identifier or rights string.
func NewGlobal(rules []config.GlobalACLRule) (*Global, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	g := &Global{}
	for _, r := range rules {
		var acl mailbox.ACL
		for _, e := range r.Entries {
			entry, err := parseGlobalEntry(e.Identifier, e.Rights)
			if err != nil {
				return nil, fmt.Errorf("acl/global: mailbox %q: %w", r.Mailbox, err)
			}
			acl = append(acl, entry)
		}
		mbox := r.Mailbox
		if mbox == "" {
			mbox = "*"
		}
		g.rules = append(g.rules, globalRule{mailbox: mbox, acl: acl})
	}
	return g, nil
}

// parseGlobalEntry builds an ACL entry from a config identifier + rights pair.
// A leading "-" on the rights string marks a negative-rights entry.
func parseGlobalEntry(identifier, rights string) (mailbox.Entry, error) {
	negative := false
	if strings.HasPrefix(rights, "-") {
		negative = true
		rights = rights[1:]
	}
	id, err := mailbox.ParseIdentifier(identifier)
	if err != nil {
		return mailbox.Entry{}, err
	}
	r, err := mailbox.ParseRights(rights)
	if err != nil {
		return mailbox.Entry{}, err
	}
	return mailbox.Entry{Identifier: id, Rights: r, Negative: negative}, nil
}

// For returns the combined global ACL applying to folder: the entries of
// every rule whose mailbox is "*" or exactly folder, in config order. Returns
// nil when no rule matches (or the receiver is nil).
func (g *Global) For(folder string) mailbox.ACL {
	if g == nil {
		return nil
	}
	var out mailbox.ACL
	for _, r := range g.rules {
		if r.mailbox == "*" || r.mailbox == folder {
			out = append(out, r.acl...)
		}
	}
	return out
}
