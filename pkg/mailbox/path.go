package mailbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// UserInfo carries the per-session storage identity for a user. The fields
// are resolved once at session start (after passdb/userdb lookup) and passed
// to MailboxBackend.OpenUser / IndexBackend.OpenUser. Backends MUST use Home
// directly and not re-derive a path from Username — UserInfo is the contract
// for "where this user's data lives".
//
// Only Username and Home are required for the current phase. The commented
// fields below are placeholders — adding a field here is backward-compatible
// (all callers pass *UserInfo by pointer; OpenUser signatures do not change).
type UserInfo struct {
	// Username is the canonical login (typically full email). Used for
	// per-user concurrency counters, log lines, and message-ID Received
	// headers — never as a storage path on its own.
	Username string

	// Home is the absolute filesystem root for the user's mailbox tree.
	// Resolved from either userdb.home (override) or the storage.mail_home
	// template expanded against the username. See Resolver.
	Home string

	// Groups is the list of supplementary groups the user belongs to,
	// sourced from the userdb `groups=` extra field (comma-separated).
	// ACL evaluation matches these against `group=<name>` and
	// `group-override=<name>` entries. Empty when not configured —
	// group= ACL entries have no effect in that case.
	Groups []string

	// Phase 3 — filesystem ownership (needed when yarilo runs as root
	// and drops privileges per-user, like Dovecot's deliver agent):
	// UID uint32
	// GID uint32

	// Phase 4 — arbitrary userdb fields forwarded from passdb result
	// (quota_rule, acl_groups, director_tag, etc.):
	// Fields map[string]string

	// Phase 5 — per-user quota enforced at storage layer:
	// QuotaBytes int64  // -1 = unlimited

	// Phase 5 — master-user impersonation context (nil = normal auth).
	// Set when the IMAP login used master-user syntax (admin\0user\0token).
	// Storage layer must not use MasterUser for path derivation.
	// MasterUser string
	// AuthToken  string
}

// Resolver maps a username (+ optional userdb override) to an absolute home
// directory using a Dovecot-style template (%u / %n / %d expansion).
//
// Dovecot reference:
//
//	mail_home = /var/mail/vhosts/%d/%n         (defaults shipped here)
//	mail_location = maildir:~/Maildir          (consumed by storage layer)
//
// When userdb returns a non-empty home value it overrides the template
// outright; otherwise the template is expanded and joined with Root.
type Resolver struct {
	// Root is prepended to template-derived paths. Absolute override paths
	// from userdb skip Root entirely.
	Root string

	// HomeTemplate is the Dovecot-style template for users with no userdb
	// override. Default: "%d/%n" (virtual hosting layout).
	HomeTemplate string
}

// Resolve returns the absolute home directory for a user. An empty
// homeOverride falls back to expanding HomeTemplate against the username
// and joining with Root.
func (r *Resolver) Resolve(username, homeOverride string) string {
	if homeOverride != "" {
		if filepath.IsAbs(homeOverride) {
			return homeOverride
		}
		return filepath.Join(r.Root, homeOverride)
	}
	tmpl := r.HomeTemplate
	if tmpl == "" {
		tmpl = "%d/%n"
	}
	return filepath.Join(r.Root, ExpandVars(tmpl, username))
}

// UserInfo builds a fully-resolved UserInfo by running Resolve against the
// supplied username + userdb override.
func (r *Resolver) UserInfo(username, homeOverride string) *UserInfo {
	return &UserInfo{
		Username: username,
		Home:     r.Resolve(username, homeOverride),
	}
}

// ExpandVars rewrites Dovecot %-variables against a username.
//
//	%u → full username                  (alice@example.com)
//	%n → local part (before @)          (alice)
//	%d → domain    (after @)            (example.com)
//	%%   → literal %
//
// Unknown sequences pass through unchanged. A bare trailing % is preserved.
func ExpandVars(template, username string) string {
	if template == "" {
		return ""
	}
	local, domain := splitUser(username)
	var b strings.Builder
	b.Grow(len(template))
	for i := 0; i < len(template); i++ {
		if template[i] != '%' || i+1 >= len(template) {
			b.WriteByte(template[i])
			continue
		}
		switch template[i+1] {
		case 'u':
			b.WriteString(username)
		case 'n':
			b.WriteString(local)
		case 'd':
			b.WriteString(domain)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(template[i+1])
		}
		i++
	}
	return b.String()
}

func splitUser(u string) (local, domain string) {
	if i := strings.IndexByte(u, '@'); i >= 0 {
		return u[:i], u[i+1:]
	}
	return u, ""
}

// Location is a parsed namespace storage URL: "driver:path".
// Currently only the "maildir" driver is supported; other drivers
// (dbox, mdbox) are accepted by the parser for forward-compat but
// must match the globally configured cfg.Storage.Mailbox driver —
// per-namespace driver mixing is deferred until backends gain a
// shared OpenNamespace dispatch.
type Location struct {
	Driver string // "maildir", "sdbox" (alias: "dbox"), "mdbox"
	Path   string // expanded absolute path (varexpand applied)
}

// ParseLocation parses "driver:path" into Location. When loc is empty
// returns (Location{}, false) so callers can detect "namespace not
// configured for storage" and fall through to NS-1a wire-only mode.
// Path is %u/%n/%d/%h-expanded against ui — when ui is nil expansion
// is a no-op (callers without a user context, e.g. system-wide
// shared/public namespaces, pass nil and ship a literal absolute path).
func ParseLocation(loc string, ui *UserInfo) (Location, bool, error) {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return Location{}, false, nil
	}
	idx := strings.IndexByte(loc, ':')
	if idx < 0 {
		return Location{}, false, fmt.Errorf("mailbox: location %q must be \"driver:path\"", loc)
	}
	driver := strings.ToLower(loc[:idx])
	path := loc[idx+1:]
	switch driver {
	case "maildir", "sdbox", "dbox", "mdbox":
		// recognised
	default:
		return Location{}, false, fmt.Errorf("mailbox: unknown storage driver %q in %q", driver, loc)
	}
	if ui != nil {
		path = ExpandVars(path, ui.Username)
		path = strings.ReplaceAll(path, "%h", ui.Home)
	} else {
		// System-wide namespace (shared/public) — strip %h gracefully
		// so a misconfigured shared namespace doesn't blow up.
		path = strings.ReplaceAll(path, "%h", "")
		path = ExpandVars(path, "")
	}
	if path == "" {
		return Location{}, false, fmt.Errorf("mailbox: location %q has empty path after expansion", loc)
	}
	return Location{Driver: driver, Path: path}, true, nil
}
