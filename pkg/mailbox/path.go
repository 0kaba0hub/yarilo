package mailbox

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"
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
	// index directory. Template vars (%u/%n/%d/%h) are already expanded
	// by the time this field is populated.
	VolatileDir string

	// IndexDir, when non-empty, redirects all per-folder index files
	// (yarilo.index*, yarilo-acl) to a separate directory tree. The
	// mailbox data (Maildir cur/new/tmp) stays under Home; only index
	// state moves. Template vars (%u/%n/%d/%h) are already expanded.
	IndexDir string

	// ControlDir, when non-empty, redirects per-folder control files
	// (yarilo-uidlist, subscriptions) to a separate directory tree.
	// The mailbox data and index files are unaffected.
	// Template vars (%u/%n/%d/%h) are already expanded.
	ControlDir string

	// AltDir, when non-empty, enables two-tier maildir storage. Messages
	// that have been cold-tiered (via altmove) live under AltDir instead
	// of Home. Reads check both primary (Home) and alt; writes always go
	// to the primary tier. Template vars (%u/%n/%d/%h) are already
	// expanded.
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

	// MailPath, when non-empty, is the root of the mailbox tree used for
	// actual mail storage. Separates the mail root from Home (which holds
	// Sieve scripts and other per-user metadata). When empty, backends
	// fall back to Home for backward compatibility.
	// Template vars (%u/%n/%d) and ~/  are already expanded.
	MailPath string

	// InboxPath, when non-empty, overrides the location of INBOX within
	// the mailbox tree. Defaults to MailPath (or Home) when empty.
	// Template vars (%u/%n/%d) and ~/ are already expanded.
	InboxPath string

	// Driver, when non-empty, names the mailbox backend driver for this
	// user ("maildir", "sdbox", "mdbox"). Populated from the driver
	// prefix of the user's mail_location (e.g. "mdbox:~/mdbox:..." → "mdbox").
	// Empty means use the globally-configured backend.
	Driver string

	// Phase 3 — filesystem ownership (needed when yarilo runs as root
	// and drops privileges per-user):
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
// directory using a template (%u / %n / %d expansion).
//
// Default template:
//
//	mail_home = /var/mail/vhosts/%d/%n
//	mail_location = maildir:~/Maildir
//
// When userdb returns a non-empty home value it overrides the template
// outright; otherwise the template is expanded and joined with Root.
type Resolver struct {
	// Root is prepended to template-derived paths. Absolute override paths
	// from userdb skip Root entirely.
	Root string

	// HomeTemplate is the path template for users with no userdb
	// override. Default: "%d/%u" (virtual hosting layout).
	HomeTemplate string

	// DefaultQuotaRules are applied to every UserInfo produced by this
	// Resolver when the userdb lookup provides no per-user override.
	DefaultQuotaRules []string

	// DefaultVolatileDir is the cluster-wide VOLATILEDIR template applied
	// when no per-user override arrives from userdb. Supports the same
	// %u/%n/%d/%h variables as HomeTemplate. Empty disables volatile dir
	// (default).
	DefaultVolatileDir string

	// DefaultIndexDir is the cluster-wide INDEX= template applied when no
	// per-user override arrives from userdb. Supports %u/%n/%d/%h. Empty
	// keeps index files co-located with the mailbox (default).
	DefaultIndexDir string

	// DefaultControlDir is the cluster-wide CONTROL= template applied when
	// no per-user override arrives from userdb. Supports %u/%n/%d/%h.
	// Empty keeps control files co-located with the mailbox (default).
	DefaultControlDir string

	// DefaultAltDir is the cluster-wide ALT= template applied when no
	// per-user override arrives from userdb. Supports %u/%n/%d/%h.
	// Empty disables two-tier storage (default).
	DefaultAltDir string

	// DefaultMailPath is the cluster-wide mail root template applied when
	// no per-user mail_path override arrives from userdb. Supports
	// %u/%n/%d/%h and ~/ expansion. Empty keeps MailPath == Home.
	DefaultMailPath string
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
	if r.DefaultMailPath != "" {
		mp := ExpandHome(r.DefaultMailPath, home)
		mp = strings.ReplaceAll(mp, "%h", home)
		ui.MailPath = ExpandVars(mp, username)
	}
	return ui
}

// ParseMailLocationMods parses the modifier section of a mail
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

// ExpandHome replaces a leading "~/" with home + "/". Paths that do not
// start with "~/" are returned unchanged. Used for mail_path / dir templates
// that follow the Dovecot ~/… convention.
func ExpandHome(path, home string) string {
	if strings.HasPrefix(path, "~/") {
		return home + path[1:]
	}
	return path
}

// ExpandVars rewrites mail_location %-variables against a username.
//
//	%u → full username                  (alice@example.com)
//	%n → local part (before @)          (alice)
//	%d → domain    (after @)            (example.com)
//	%%   → literal %
//
//	%<width>.<modulo>N<var> → hash-directory sharding variable.
//	  MD5(<var>) → first 4 bytes as big-endian uint32 → mod <modulo>
//	  → zero-padded hex string of length <width>.
//	  Example: %2.256Nu with "u1@d00001.test" → "76"
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
		// Hash variable: %<width>.<modulo>N<u|n|d>
		if c := template[i+1]; c >= '1' && c <= '9' {
			if s, adv, ok := expandHashVar(template[i+1:], username, local, domain); ok {
				b.WriteString(s)
				i += adv
				continue
			}
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

// expandHashVar tries to parse and expand a hash-directory sequence of the
// form "<width>.<modulo>N<var>" (the leading % has already been consumed by
// the caller). Returns the expanded string, the number of bytes consumed
// (not counting the %), and true on success.
func expandHashVar(s, username, local, domain string) (string, int, bool) {
	// Parse width digits.
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 || j >= len(s) || s[j] != '.' {
		return "", 0, false
	}
	width, _ := strconv.ParseUint(s[:j], 10, 32)
	j++ // skip '.'

	// Parse modulo digits.
	k := j
	for k < len(s) && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k == j || k >= len(s) || s[k] != 'N' {
		return "", 0, false
	}
	modulo, _ := strconv.ParseUint(s[j:k], 10, 32)
	if modulo == 0 {
		return "", 0, false
	}
	k++ // skip 'N'

	if k >= len(s) {
		return "", 0, false
	}
	var subject string
	switch s[k] {
	case 'u':
		subject = username
	case 'n':
		subject = local
	case 'd':
		subject = domain
	default:
		return "", 0, false
	}

	sum := md5.Sum([]byte(subject))
	n := binary.BigEndian.Uint32(sum[:4]) % uint32(modulo)
	result := fmt.Sprintf("%0*x", width, n)
	return result, k + 1, true // k+1: consumed bytes after the '%'
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
	Driver      string // "maildir", "sdbox" (alias: "dbox"), "mdbox"
	Path        string // expanded absolute path (varexpand applied)
	IndexDir    string // INDEX= modifier, expanded; empty = co-located
	VolatileDir string // VOLATILEDIR= modifier, expanded; empty = default
	ControlDir  string // CONTROL= modifier, expanded; empty = co-located
	AltDir      string // ALT= modifier, expanded; empty = disabled
}

// ParseLocation parses "driver:path[:KEY=value:...]" into Location.
// When loc is empty returns (Location{}, false) so callers can detect
// "namespace not configured for storage" and fall through to NS-1a
// wire-only mode. Path and option values are %u/%n/%d/%h-expanded
// against ui — when ui is nil expansion is a no-op (callers without a
// user context, e.g. system-wide shared/public namespaces, pass nil and
// ship literal absolute paths). Unknown options are silently ignored for
// forward compatibility.
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
	switch driver {
	case "maildir", "sdbox", "dbox", "mdbox":
		// recognised
	default:
		return Location{}, false, fmt.Errorf("mailbox: unknown storage driver %q in %q", driver, loc)
	}

	expand := func(s string) string {
		if ui != nil {
			s = ExpandVars(s, ui.Username)
			return strings.ReplaceAll(s, "%h", ui.Home)
		}
		s = strings.ReplaceAll(s, "%h", "")
		return ExpandVars(s, "")
	}

	parts := strings.Split(loc[idx+1:], ":")
	path := expand(parts[0])
	if path == "" {
		return Location{}, false, fmt.Errorf("mailbox: location %q has empty path after expansion", loc)
	}

	out := Location{Driver: driver, Path: path}
	for _, opt := range parts[1:] {
		eq := strings.IndexByte(opt, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToUpper(opt[:eq])
		val := expand(opt[eq+1:])
		switch key {
		case "INDEX":
			out.IndexDir = val
		case "VOLATILEDIR":
			out.VolatileDir = val
		case "CONTROL":
			out.ControlDir = val
		case "ALT":
			out.AltDir = val
			// unknown keys are silently ignored
		}
	}
	return out, true, nil
}
