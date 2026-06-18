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

	// VolatileDir, when non-empty, redirects volatile index artefacts
	// (Recreate tmp files) to a local path instead of the NFS-backed
	// index directory. Mirrors Dovecot's VOLATILEDIR mail-location
	// modifier. Template vars (%u/%n/%d/%h) are already expanded by the
	// time this field is populated.
	VolatileDir string

	// IndexDir, when non-empty, redirects all per-folder index files
	// (yarilo.index*, yarilo-acl) to a separate directory tree. The
	// mailbox data (Maildir cur/new/tmp) stays under Home; only index
	// state moves. Mirrors Dovecot's INDEX= mail-location modifier.
	// Template vars (%u/%n/%d/%h) are already expanded.
	IndexDir string

	// ControlDir, when non-empty, redirects per-folder control files
	// (yarilo-uidlist, subscriptions) to a separate directory tree.
	// The mailbox data and index files are unaffected. Mirrors Dovecot's
	// CONTROL= mail-location modifier.
	// Template vars (%u/%n/%d/%h) are already expanded.
	ControlDir string

	// AltDir, when non-empty, enables two-tier maildir storage. Messages
	// that have been cold-tiered (via altmove) live under AltDir instead
	// of Home. Reads check both primary (Home) and alt; writes always go
	// to the primary tier. Mirrors Dovecot's ALT= mail-location modifier.
	// Template vars (%u/%n/%d/%h) are already expanded.
	AltDir string

	// Groups is the list of supplementary groups the user belongs to,
	// sourced from the userdb `groups=` extra field (comma-separated).
	// ACL evaluation matches these against `group=<name>` and
	// `group-override=<name>` entries. Empty when not configured —
	// group= ACL entries have no effect in that case.
	Groups []string

	// QuotaRules is the list of per-user quota rules sourced from the
	// userdb `quota_rule=` extra field. Format: `*:storage=5G` or
	// `*:messages=100000`. Empty means no quota limit.
	QuotaRules []string

	// SessionID is the IMAP/POP3 session identifier assigned by the login
	// proxy. Included in the yarilo-locks owner string for BUSY diagnostics.
	// Empty for LMTP and other non-session contexts.
	SessionID string

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
	// override. Default: "%d/%u" (virtual hosting layout).
	HomeTemplate string

	// DefaultQuotaRules are applied to every UserInfo produced by this
	// Resolver when the userdb lookup provides no per-user override.
	// Mirrors Dovecot's quota_rule setting at the global level.
	DefaultQuotaRules []string

	// DefaultVolatileDir is the cluster-wide VOLATILEDIR template applied
	// when no per-user override arrives from userdb. Supports the same
	// %u/%n/%d/%h variables as HomeTemplate. Empty disables volatile dir
	// (default). Mirrors Dovecot's VOLATILEDIR mail-location modifier at
	// the global config level.
	DefaultVolatileDir string

	// DefaultIndexDir is the cluster-wide INDEX= template applied when no
	// per-user override arrives from userdb. Supports %u/%n/%d/%h. Empty
	// keeps index files co-located with the mailbox (default). Mirrors
	// Dovecot's INDEX= mail-location modifier at the global config level.
	DefaultIndexDir string

	// DefaultControlDir is the cluster-wide CONTROL= template applied when
	// no per-user override arrives from userdb. Supports %u/%n/%d/%h.
	// Empty keeps control files co-located with the mailbox (default).
	// Mirrors Dovecot's CONTROL= mail-location modifier at the global
	// config level.
	DefaultControlDir string

	// DefaultAltDir is the cluster-wide ALT= template applied when no
	// per-user override arrives from userdb. Supports %u/%n/%d/%h.
	// Empty disables two-tier storage (default). Mirrors Dovecot's
	// ALT= mail-location modifier at the global config level.
	DefaultAltDir string
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
		tmpl = "%d/%u"
	}
	return filepath.Join(r.Root, ExpandVars(tmpl, username))
}

// UserInfo builds a fully-resolved UserInfo by running Resolve against the
// supplied username + userdb override. DefaultVolatileDir (if set) is
// expanded with %u/%n/%d/%h variables and stored in VolatileDir; per-user
// overrides can overwrite this field after the call returns.
func (r *Resolver) UserInfo(username, homeOverride string) *UserInfo {
	home := r.Resolve(username, homeOverride)
	ui := &UserInfo{
		Username:   username,
		Home:       home,
		QuotaRules: r.DefaultQuotaRules,
	}
	if r.DefaultVolatileDir != "" {
		vd := strings.ReplaceAll(r.DefaultVolatileDir, "%h", home)
		ui.VolatileDir = ExpandVars(vd, username)
	}
	if r.DefaultIndexDir != "" {
		id := strings.ReplaceAll(r.DefaultIndexDir, "%h", home)
		ui.IndexDir = ExpandVars(id, username)
	}
	if r.DefaultControlDir != "" {
		cd := strings.ReplaceAll(r.DefaultControlDir, "%h", home)
		ui.ControlDir = ExpandVars(cd, username)
	}
	if r.DefaultAltDir != "" {
		ad := strings.ReplaceAll(r.DefaultAltDir, "%h", home)
		ui.AltDir = ExpandVars(ad, username)
	}
	return ui
}

// ParseMailLocationMods parses the modifier section of a Dovecot mail
// location string of the form "driver:path:KEY1=v1:KEY2=v2". Returns a
// map of uppercase modifier keys to their raw (unexpanded) values.
// Returns nil when the string has fewer than three colon-separated segments
// (i.e. no modifiers present).
func ParseMailLocationMods(loc string) map[string]string {
	parts := strings.Split(loc, ":")
	if len(parts) < 3 {
		return nil
	}
	mods := make(map[string]string, len(parts)-2)
	for _, p := range parts[2:] {
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			mods[strings.ToUpper(p[:eq])] = p[eq+1:]
		}
	}
	return mods
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
