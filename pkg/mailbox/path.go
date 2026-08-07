package mailbox

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// UserInfo carries the per-session storage identity for a user, resolved once
// at session start (after passdb/userdb lookup) and passed to
// MailboxBackend.OpenUser / IndexBackend.OpenUser. Backends MUST use Home
// directly and never re-derive a path from Username.
//
// Only Username and Home are required. Adding a field is backward-compatible —
// callers pass *UserInfo by pointer and OpenUser signatures do not change.
type UserInfo struct {
	// Username is the canonical login (typically full email). Used for
	// per-user concurrency counters, log lines, and Received headers — never
	// as a storage path on its own.
	Username string

	// Home is the absolute filesystem root for the user's mailbox tree,
	// resolved from userdb.home (override) or the storage.mail_home template
	// expanded against the username. See Resolver.
	Home string

	// VolatileDir, when non-empty, redirects volatile index artefacts
	// (Recreate tmp files) to a local path instead of the NFS-backed index
	// directory. Template vars (%u/%n/%d/%h) are already expanded.
	VolatileDir string

	// IndexDir, when non-empty, redirects per-folder index files
	// (yarilo.index*, yarilo-acl) to a separate tree; mailbox data stays under
	// Home. Template vars (%u/%n/%d/%h) are already expanded.
	IndexDir string

	// ControlDir, when non-empty, redirects per-folder control files
	// (yarilo-uidlist, subscriptions) to a separate tree; mailbox data and
	// index files are unaffected. Template vars (%u/%n/%d/%h) are already
	// expanded.
	ControlDir string

	// AltDir, when non-empty, enables two-tier maildir storage: cold-tiered
	// (altmove) messages live under AltDir. Reads check both tiers; writes
	// always go to the primary (Home). Template vars (%u/%n/%d/%h) are already
	// expanded.
	AltDir string

	// Groups is the user's supplementary groups from the userdb `groups=`
	// field (comma-separated). ACL evaluation matches them against
	// `group=<name>` / `group-override=<name>` entries. Empty disables those.
	Groups []string

	// ACLUser / ACLGroups override the ACL evaluation identity (acl_user /
	// acl_groups userdb fields), typically on a master-user session. Empty
	// ACLUser means evaluate as Username / Groups.
	ACLUser   string
	ACLGroups []string

	// QuotaRules is the per-user quota rules from the userdb `quota_rule=`
	// field. Format: `*:storage=5G` or `*:messages=100000`. Empty means no
	// quota limit.
	QuotaRules []string

	// QuotaOverFlag is the userdb `quota_over_flag` value the
	// quota_over_status check reconciles against actual usage at login.
	QuotaOverFlag string

	// SessionID is the IMAP/POP3 session identifier from the login proxy,
	// included in the yarilo-locks owner string for BUSY diagnostics. Empty
	// for LMTP and other non-session contexts.
	SessionID string

	// MailPath, when non-empty, is the mail storage root, separated from Home
	// (which holds Sieve scripts and other metadata). Empty falls back to
	// Home. Template vars (%u/%n/%d) and ~/ are already expanded.
	MailPath string

	// InboxPath, when non-empty, overrides the INBOX location within the
	// mailbox tree. Defaults to MailPath (or Home). Template vars (%u/%n/%d)
	// and ~/ are already expanded.
	InboxPath string

	// Driver, when non-empty, names the mailbox backend driver ("maildir",
	// "sdbox", "mdbox"), from the driver prefix of the user's mail_location.
	// Empty uses the globally-configured backend.
	Driver string

	// Separator is the IMAP hierarchy separator for this namespace. Folder
	// names carry it; each backend converts to its on-disk separator (maildir
	// "." flat, dbox "/" nested) via mailbox.FolderSubpath. Empty defaults to
	// "/".
	Separator string

	// StorageEscapeChar keeps a folder name literal on disk when the layout
	// would reinterpret it: a single character, or "" for no escaping. Carried
	// on UserInfo so every tree derived from the name -- mail, index, control,
	// FTS -- escapes identically. A tree that skipped it would name the same
	// mailbox differently from the others (#1078).
	StorageEscapeChar string

	// SkipNFCNormalize turns off the NFC normalisation that turning a folder
	// name into a storage name otherwise applies. Inverted on purpose: the
	// config key (mailbox_list_normalize_to_nfc) defaults to true, so the zero
	// value of this field has to mean "normalise". A NormalizeNFC bool would
	// disable it for every UserInfo built without the field being set --
	// exactly the silent divergence this exists to remove (#1092).
	SkipNFCNormalize bool

	// Phase 3 — filesystem ownership (yarilo runs as root, drops per-user):
	// UID uint32
	// GID uint32

	// Phase 4 — arbitrary userdb fields forwarded from passdb result:
	// Fields map[string]string

	// Phase 5 — per-user quota enforced at storage layer:
	// QuotaBytes int64  // -1 = unlimited

	// Phase 5 — master-user impersonation context (nil = normal auth).
	// Storage layer must not use MasterUser for path derivation.
	// MasterUser string
	// AuthToken  string
}

// ACLIdentity returns the (user, groups) pair to evaluate ACLs against: the
// acl_user / acl_groups override when set, else the session's Username / Groups.
func (u *UserInfo) ACLIdentity() (string, []string) {
	if u.ACLUser != "" {
		return u.ACLUser, u.ACLGroups
	}
	return u.Username, u.Groups
}

// Resolver maps a username (+ optional userdb override) to an absolute home
// directory via a %u/%n/%d template. A non-empty userdb home overrides the
// template outright; otherwise the template is expanded and joined with Root.
type Resolver struct {
	// Root is prepended to template-derived paths. Absolute userdb overrides
	// skip Root entirely.
	Root string

	// HomeTemplate is the path template for users with no userdb override.
	// Default: "%d/%u" (virtual hosting layout).
	HomeTemplate string

	// DefaultQuotaRules apply to every UserInfo when userdb provides no
	// per-user override.
	DefaultQuotaRules []string

	// DefaultVolatileDir is the cluster-wide VOLATILEDIR template (%u/%n/%d/%h)
	// applied when no userdb override arrives. Empty disables volatile dir.
	DefaultVolatileDir string

	// DefaultIndexDir is the cluster-wide INDEX= template (%u/%n/%d/%h) applied
	// when no userdb override arrives. Empty co-locates index files.
	DefaultIndexDir string

	// DefaultControlDir is the cluster-wide CONTROL= template (%u/%n/%d/%h)
	// applied when no userdb override arrives. Empty co-locates control files.
	DefaultControlDir string

	// DefaultAltDir is the cluster-wide ALT= template (%u/%n/%d/%h) applied
	// when no userdb override arrives. Empty disables two-tier storage.
	DefaultAltDir string

	// DefaultMailPath is the cluster-wide mail root template (%u/%n/%d/%h, ~/)
	// applied when no userdb mail_path override arrives. Empty keeps
	// MailPath == Home.
	DefaultMailPath string

	// DefaultStorageEscapeChar is stamped onto every UserInfo, so the mail,
	// index, control and FTS trees all escape identically. A tree built with a
	// different setting would name the same mailbox differently from the rest
	// (#1078).
	DefaultStorageEscapeChar string

	// DefaultSkipNFCNormalize is stamped onto every UserInfo alongside the
	// escape character, for the same reason: one derivation, one answer.
	DefaultSkipNFCNormalize bool

	// DefaultSeparator is the IMAP hierarchy separator stamped onto every
	// UserInfo; the IMAP session overrides it per-namespace. Empty defaults to
	// "/" at the point of use.
	DefaultSeparator string
}

// Resolve returns the absolute home directory. An empty homeOverride expands
// HomeTemplate against the username and joins with Root.
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

// UserInfo builds a fully-resolved UserInfo from the username + userdb
// override. The Default* templates (if set) are ~/-, %h- and %u/%n/%d-expanded
// into their fields; per-user overrides may overwrite them after the call.
func (r *Resolver) UserInfo(username, homeOverride string) *UserInfo {
	home := r.Resolve(username, homeOverride)
	ui := &UserInfo{
		Username:   username,
		Home:       home,
		QuotaRules: r.DefaultQuotaRules,
		Separator:  r.DefaultSeparator,

		StorageEscapeChar: r.DefaultStorageEscapeChar,
		SkipNFCNormalize:  r.DefaultSkipNFCNormalize,
	}
	if r.DefaultVolatileDir != "" {
		ui.VolatileDir = ExpandLocation(r.DefaultVolatileDir, home, username)
	}
	if r.DefaultIndexDir != "" {
		ui.IndexDir = ExpandLocation(r.DefaultIndexDir, home, username)
	}
	if r.DefaultControlDir != "" {
		ui.ControlDir = ExpandLocation(r.DefaultControlDir, home, username)
	}
	if r.DefaultAltDir != "" {
		ui.AltDir = ExpandLocation(r.DefaultAltDir, home, username)
	}
	if r.DefaultMailPath != "" {
		ui.MailPath = ExpandLocation(r.DefaultMailPath, home, username)
	}
	return ui
}

// ExpandLocation resolves a storage location template: a leading "~/" and "%h"
// both stand for the user's home, then %u/%n/%d. Every location value goes
// through it, from the config or the userdb, so one string means one path.
func ExpandLocation(tmpl, home, username string) string {
	v := ExpandHome(tmpl, home)
	return ExpandVars(strings.ReplaceAll(v, "%h", home), username)
}

// ParseMailLocationMods parses the modifier section of a
// "driver:path:KEY1=v1:KEY2=v2" location into a map of uppercase keys to raw
// (unexpanded) values. Returns nil when there are fewer than three
// colon-separated segments (no modifiers).
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

// ExpandHome replaces a leading "~/" with home + "/", else returns path
// unchanged. Used for mail_path / dir templates in the ~/… convention.
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

// Location is a parsed namespace storage URL "driver:path". Drivers other than
// the active one are accepted by the parser for forward-compat but must match
// the globally configured cfg.Storage.Mailbox driver; per-namespace driver
// mixing is deferred until backends gain a shared OpenNamespace dispatch.
type Location struct {
	Driver      string // "maildir", "sdbox" (alias: "dbox"), "mdbox"
	Path        string // expanded absolute path (varexpand applied)
	IndexDir    string // INDEX= modifier, expanded; empty = co-located
	VolatileDir string // VOLATILEDIR= modifier, expanded; empty = default
	ControlDir  string // CONTROL= modifier, expanded; empty = co-located
	AltDir      string // ALT= modifier, expanded; empty = disabled
}

// recognisedDriver reports whether name is a storage driver yarilo knows.
func recognisedDriver(name string) bool {
	switch name {
	case "maildir", "sdbox", "dbox", "mdbox":
		return true
	default:
		return false
	}
}

// LocationDriver returns the lowercased storage driver named by a mail_location
// ("mdbox:~/mdbox" → "mdbox") when it is a recognised driver, or "" otherwise.
// Unlike ParseLocation it does not require a path, so it accepts the bare
// "driver:" form whose path is derived from the user's home.
func LocationDriver(loc string) string {
	idx := strings.IndexByte(strings.TrimSpace(loc), ':')
	if idx <= 0 {
		return ""
	}
	driver := strings.ToLower(strings.TrimSpace(loc)[:idx])
	if !recognisedDriver(driver) {
		return ""
	}
	return driver
}

// ParseLocation parses "driver:path[:KEY=value:...]" into a Location. Empty loc
// returns (Location{}, false) so callers can fall through to wire-only mode.
// Path and option values are %u/%n/%d/%h-expanded against ui (a nil ui makes
// expansion a no-op). Unknown options are ignored for forward compatibility.
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
	if !recognisedDriver(driver) {
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
