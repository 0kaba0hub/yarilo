package protocol

import (
	"sort"
	"strconv"
	"strings"
)

// UserInfo carries the full set of user fields a userdb lookup can
// return. Modelled after Dovecot 2.4's auth-fields plus the reserved
// field names every userdb driver writes (auth-fields.c,
// auth-request.c, userdb-passwd.c). The struct is intentionally
// comprehensive — adding fields later means breaking the protocol
// surface that pkg/authclient + yarilo-backend-api speak, so the
// shape is fixed up-front during Phase AUTH-1 and the prefix /
// snapshot / prefetch semantics in later phases extend BEHAVIOUR
// without touching DATA shape.
//
// Zero values are the "field absent" marker; the master-protocol
// wire serialiser in Phase AUTH-1 PR 2 skips them. Sensitive fields
// (Password, CertName, PolicyResponse) are populated server-side but
// the wire layer filters them out unconditionally — they exist on
// the struct so internal pipelines can carry them without a parallel
// struct.
type UserInfo struct {
	// ---- Identity & delegation --------------------------------

	// Username is the resolved canonical username this UserInfo
	// describes. Mandatory.
	Username string

	// OriginalUser is the username the client supplied before any
	// userdb-side translations (case folding, alias rewrite). Empty
	// when no translation occurred.
	OriginalUser string

	// MasterUser is the master user when this lookup arrived via
	// `master=` delegation. Populated only by Phase AUTH-3 master-
	// user flows; the field exists in PR 1 so the schema is stable.
	MasterUser string

	// LoginUser is the post-impersonation effective user. For non-
	// delegated lookups this equals Username; for master-user flows
	// it is the target user. Phase AUTH-3 populates this.
	LoginUser string

	// ---- System identity --------------------------------------

	UID               uint32
	GID               uint32
	Home              string
	Chroot            string
	SystemGroupsUser  string   // override username for system group lookup
	Groups            []string // supplementary group names
	ClientCertPresent bool     // TLS client cert auth was used

	// ---- Mail storage -----------------------------------------

	// MailLocation overrides the global mail_location (Dovecot
	// `mail` field).
	MailLocation      string
	MailUID           uint32 // distinct from system UID
	MailGID           uint32 // distinct from system GID
	MailboxFormat     string // maildir | sdbox | mdbox
	MailAttributeDict string // dict URL for RFC 5464 METADATA

	// VolatileDir is the VOLATILEDIR modifier extracted from MailLocation
	// (or set directly via the `volatile_dir` userdb extra field). Carries
	// the raw template string (%u/%n/%d/%h not yet expanded) so callers
	// can expand it against the resolved home after userdb completes.
	VolatileDir string

	// IndexDir is the INDEX= modifier extracted from MailLocation (or set
	// directly via the `index_dir` userdb extra field). Carries the raw
	// template string (%u/%n/%d/%h not yet expanded). When set, per-folder
	// index files (yarilo.index*, yarilo-acl) are stored here instead of
	// co-located with the mailbox data under Home.
	IndexDir string

	// ControlDir is the CONTROL= modifier extracted from MailLocation (or
	// set directly via the `control_dir` userdb extra field). Carries the
	// raw template string (%u/%n/%d/%h not yet expanded). When set,
	// per-folder control files (yarilo-uidlist, subscriptions) are stored
	// here instead of co-located with the mailbox data under Home.
	ControlDir string

	// AltDir is the ALT= modifier extracted from MailLocation (or set
	// directly via the `alt_dir` userdb extra field). Carries the raw
	// template string (%u/%n/%d/%h not yet expanded). When set, messages
	// that have been cold-tiered live under AltDir; reads check both
	// primary (Home) and alt tiers.
	AltDir string

	// ---- Quota ------------------------------------------------

	// QuotaRules is the list of per-user quota rules (Dovecot
	// `quota_rule=` can appear multiple times for nested roots).
	QuotaRules    []string
	QuotaOverFlag string // operator-set override marker (string sentinel)

	// ---- Network access ---------------------------------------

	// AllowNets is the list of IP / CIDR strings the user is
	// allowed to authenticate from. Empty means no IP restriction.
	AllowNets []string

	// ---- Login control / fail shape ---------------------------

	NoLogin        bool // reject login outright
	NoDelay        bool // bypass auth-penalty backoff (Phase AUTH-4)
	NoAuthenticate bool // auth disabled for this user
	PassExpired    bool // password expired — client must re-set
	NoPassword     bool // passdb without a password (e.g. OAuth)

	// ---- Proxy ------------------------------------------------

	Proxy               bool
	ProxyMaybe          bool   // proxy only when remote host differs
	Host                string // proxy backend host
	Port                int    // proxy backend port (0 = default)
	DestUser            string // override target user in proxy
	ProxyMech           string // SASL mech to use against backend
	ProxyTimeout        int    // seconds
	ProxyRedirectReauth bool
	ProxyNoPipelining   bool
	SSL                 string // yes | any | required
	StartTLS            bool

	// ---- Connection limits ------------------------------------

	MailMaxUserIPConnections int // per-(user, IP)
	MailMaxUserConnections   int // per-user across IPs

	// ---- Audit / event ----------------------------------------

	Service   string // IMAP | POP3 | LMTP | submission ...
	LocalName string // SNI hostname the client connected to

	// ---- Forwarding (proxy chain) -----------------------------

	// Forward carries `forward_*` fields that are passed through
	// a proxy chain unchanged. Map key is the field name WITHOUT
	// the `forward_` prefix.
	Forward map[string]string

	// ---- Extra (forward-compat / driver-specific) -------------

	// Extra is the catch-all bag for fields not modelled as typed
	// struct members. Anything a userdb driver returns that does
	// not map to a typed field above ends up here, keyed by the
	// raw field name. Wire-level prefix semantics (`userdb_*=`,
	// `auth_*=`) land in Phase AUTH-2.
	Extra map[string]string

	// ---- Internal (never serialised to the wire) --------------

	// Password is the stored credential (or scheme-encoded hash)
	// from a passdb lookup. The master-protocol wire serialiser
	// in Phase AUTH-1 PR 2 strips this unconditionally so an
	// admin-side USER request can never leak credentials. Present
	// on the struct so passdb→userdb internal pipelines do not
	// need a parallel type.
	Password string

	// CertName is the TLS client certificate Common Name. Set by
	// the EXTERNAL SASL mechanism (Phase AUTH-5). Internal-only.
	CertName string

	// PolicyResponse is the verbatim body returned by the policy
	// HTTP endpoint in Phase AUTH-6. Internal-only.
	PolicyResponse string
}

// Userdb is the contract every userdb backend implements. Unlike
// Passdb.Authenticate, Lookup takes no password — userdb answers
// "who is this user?" for admin tooling, master-protocol clients,
// and the prefetch shortcut (Phase AUTH-2) that lets a passdb
// short-circuit a redundant userdb round-trip.
//
// Semantics on the return value match Passdb / Chain:
//   - (ui != nil, nil)  — user found, ui populated
//   - (nil, nil)        — user unknown in this backend; UserdbChain
//     tries the next entry
//   - (nil, err != nil) — backend failure; UserdbChain stops and
//     surfaces the error to the caller
type Userdb interface {
	Lookup(username string) (*UserInfo, error)
}

// UserdbChain composes multiple Userdb backends with first-hit-wins
// semantics, mirroring the Passdb Chain. The first non-nil response
// from any backend in the chain is returned; an error from any
// backend short-circuits the chain (matches Dovecot's
// USERDB_RESULT_INTERNAL_FAILURE — "stop, do not try fallbacks").
type UserdbChain []Userdb

func (c UserdbChain) Lookup(username string) (*UserInfo, error) {
	for _, db := range c {
		ui, err := db.Lookup(username)
		if err != nil {
			return nil, err
		}
		if ui != nil {
			return ui, nil
		}
	}
	return nil, nil
}

// UserdbIterator is implemented by userdb backends that support
// listing every known user. Used by `yarilo-admin backend user
// iterate` and by the master-protocol `LIST` command. Optional —
// a backend that cannot enumerate (e.g. an LDAP search with no
// suitable filter) simply does not implement it.
type UserdbIterator interface {
	Iterate() ([]string, error)
}

// VisitFields calls fn for every non-zero typed field on UserInfo
// in canonical order. Internal-only fields (Password, CertName,
// PolicyResponse) are NEVER passed to fn — callers that serialise
// to a wire / bag get them stripped by construction.
//
// Lists are emitted as a single comma-joined value; booleans are
// emitted only when true (value "yes"); numbers are decimal-rendered.
// Forward fields emit with the `forward_` prefix; Extra entries
// emit verbatim. Forward and Extra are iterated in lexicographic
// key order so two callers serialising the same UserInfo produce
// byte-identical output.
//
// Shared by master.go's marshalUserInfo (no prefix, tab-joined
// wire form) and protocol.go's writeUserdbFields (`userdb_` prefix,
// Fields-bag form) so the field surface stays in lockstep.
func (ui *UserInfo) VisitFields(fn func(key, value string)) {
	if ui == nil {
		return
	}

	str := func(k, v string) {
		if v != "" {
			fn(k, v)
		}
	}
	num := func(k string, v uint64) {
		if v != 0 {
			fn(k, formatUint(v))
		}
	}
	signed := func(k string, v int) {
		if v != 0 {
			fn(k, formatInt(v))
		}
	}
	yes := func(k string, v bool) {
		if v {
			fn(k, "yes")
		}
	}
	list := func(k string, v []string) {
		if len(v) > 0 {
			fn(k, joinCSV(v))
		}
	}

	str("original_user", ui.OriginalUser)
	str("master_user", ui.MasterUser)
	str("login_user", ui.LoginUser)

	num("uid", uint64(ui.UID))
	num("gid", uint64(ui.GID))
	str("home", ui.Home)
	str("chroot", ui.Chroot)
	str("system_groups_user", ui.SystemGroupsUser)
	list("groups", ui.Groups)
	yes("client_cert_present", ui.ClientCertPresent)

	str("mail", ui.MailLocation)
	str("volatile_dir", ui.VolatileDir)
	str("index_dir", ui.IndexDir)
	str("control_dir", ui.ControlDir)
	str("alt_dir", ui.AltDir)
	num("mail_uid", uint64(ui.MailUID))
	num("mail_gid", uint64(ui.MailGID))
	str("mailbox_format", ui.MailboxFormat)
	str("mail_attribute_dict", ui.MailAttributeDict)

	list("quota_rule", ui.QuotaRules)
	str("quota_over_flag", ui.QuotaOverFlag)

	list("allow_nets", ui.AllowNets)

	yes("nologin", ui.NoLogin)
	yes("nodelay", ui.NoDelay)
	yes("noauthenticate", ui.NoAuthenticate)
	yes("pass_expired", ui.PassExpired)
	yes("nopassword", ui.NoPassword)

	yes("proxy", ui.Proxy)
	yes("proxy_maybe", ui.ProxyMaybe)
	str("host", ui.Host)
	signed("port", ui.Port)
	str("destuser", ui.DestUser)
	str("proxy_mech", ui.ProxyMech)
	signed("proxy_timeout", ui.ProxyTimeout)
	yes("proxy_redirect_reauth", ui.ProxyRedirectReauth)
	yes("proxy_nopipelining", ui.ProxyNoPipelining)
	str("ssl", ui.SSL)
	yes("starttls", ui.StartTLS)

	signed("mail_max_userip_connections", ui.MailMaxUserIPConnections)
	signed("mail_max_user_connections", ui.MailMaxUserConnections)

	str("service", ui.Service)
	str("local_name", ui.LocalName)

	for _, k := range sortedMapKeys(ui.Forward) {
		fn("forward_"+k, ui.Forward[k])
	}
	for _, k := range sortedMapKeys(ui.Extra) {
		fn(k, ui.Extra[k])
	}
}

// formatUint / formatInt / joinCSV / sortedMapKeys are tiny
// dependency-free helpers shared by VisitFields and the master /
// client wire serialisers. Live here (not in a separate utils
// file) because each is exactly one usage pattern and inlining
// them would bloat each call site.

func formatUint(v uint64) string { return strconv.FormatUint(v, 10) }

func formatInt(v int) string { return strconv.Itoa(v) }

func joinCSV(v []string) string { return strings.Join(v, ",") }

func sortedMapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
