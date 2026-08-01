package protocol

import (
	"sort"
	"strconv"
	"strings"
)

// UserInfo carries the full set of user fields a userdb lookup can
// return. Zero values mark an absent field and are skipped by the
// wire serialiser. Sensitive fields (Password, CertName,
// PolicyResponse) exist on the struct so passdb→userdb pipelines can
// carry them, but the wire layer filters them out unconditionally.
type UserInfo struct {
	// ---- Identity & delegation --------------------------------

	// Username is the resolved canonical username. Mandatory.
	Username string

	// OriginalUser is the username the client supplied before any
	// userdb-side translation (case folding, alias rewrite). Empty
	// when no translation occurred.
	OriginalUser string

	// MasterUser is the master user when this lookup arrived via
	// `master=` delegation.
	MasterUser string

	// LoginUser is the post-impersonation effective user. Equals
	// Username for non-delegated lookups; the target user for
	// master-user flows.
	LoginUser string

	// ---- System identity --------------------------------------

	UID               uint32
	GID               uint32
	Home              string
	Chroot            string
	SystemGroupsUser  string   // override username for system group lookup
	Groups            []string // supplementary group names
	ClientCertPresent bool     // TLS client cert auth was used

	// ACLUser / ACLGroups override the identity used when evaluating ACLs.
	// Set via the acl_user / acl_groups userdb fields. Empty ACLUser means
	// evaluate as Username / Groups.
	ACLUser   string
	ACLGroups []string

	// ---- Mail storage -----------------------------------------

	// MailLocation overrides the global mail_location.
	MailLocation      string
	MailUID           uint32 // distinct from system UID
	MailGID           uint32 // distinct from system GID
	MailboxFormat     string // maildir | sdbox | mdbox
	MailAttributeDict string // dict URL for RFC 5464 METADATA

	// VolatileDir is the VOLATILEDIR modifier from MailLocation (or the
	// `volatile_dir` userdb field). Raw template (%u/%n/%d/%h unexpanded),
	// expanded against the resolved home after userdb completes.
	VolatileDir string

	// IndexDir is the INDEX= modifier from MailLocation (or the `index_dir`
	// userdb field). Raw template (%u/%n/%d/%h unexpanded). When set,
	// per-folder index files (yarilo.index*, yarilo-acl) live here instead
	// of alongside the mailbox data under Home.
	IndexDir string

	// ControlDir is the CONTROL= modifier from MailLocation (or the
	// `control_dir` userdb field). Raw template (%u/%n/%d/%h unexpanded).
	// When set, per-folder control files (yarilo-uidlist, subscriptions)
	// live here instead of alongside the mailbox data under Home.
	ControlDir string

	// AltDir is the ALT= modifier from MailLocation (or the `alt_dir`
	// userdb field). Raw template (%u/%n/%d/%h unexpanded). Cold-tiered
	// messages live here; reads check both primary (Home) and alt tiers.
	AltDir string

	// MailPath is the root of the mail storage tree, separate from Home
	// (which holds Sieve scripts and other metadata). Set via the
	// `mail_path` userdb field or derived from the base path of
	// MailLocation. Raw template (%u/%n/%d/%h/~/ unexpanded). Empty falls
	// back to Home.
	MailPath string

	// InboxPath overrides the INBOX location within the mail tree. Set via
	// the `mail_inbox_path` userdb field. Raw template (%u/%n/%d/%h/~/
	// unexpanded). Empty defaults to MailPath (or Home).
	InboxPath string

	// ---- Quota ------------------------------------------------

	// QuotaRules is the list of per-user quota rules (`quota_rule=`
	// may appear multiple times for nested roots).
	QuotaRules    []string
	QuotaOverFlag string // operator-set override marker (string sentinel)

	// ---- Network access ---------------------------------------

	// AllowNets is the list of IP / CIDR strings the user is
	// allowed to authenticate from. Empty means no IP restriction.
	AllowNets []string

	// ---- Director routing ---------------------------------------

	// DirectorTag shards this user to a specific director backend tag
	// (`director_tag=` field). A per-user value always wins over the login
	// component's static director_tag ("" is the untagged pool), so one
	// shared login fleet can route different users to different tag-pools.
	DirectorTag string

	// ---- Login control / fail shape ---------------------------

	NoLogin        bool // reject login outright
	NoDelay        bool // bypass auth-penalty backoff
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

	// Forward carries `forward_*` fields passed through a proxy chain
	// unchanged. Map key is the field name WITHOUT the `forward_` prefix.
	Forward map[string]string

	// ---- Extra (forward-compat / driver-specific) -------------

	// Extra is the catch-all bag for fields not modelled as typed struct
	// members, keyed by the raw field name.
	Extra map[string]string

	// ---- Internal (never serialised to the wire) --------------

	// Password is the stored credential (or scheme-encoded hash) from a
	// passdb lookup. The wire serialiser strips it unconditionally so an
	// admin-side USER request cannot leak credentials.
	Password string

	// CertName is the TLS client certificate Common Name, set by the
	// EXTERNAL SASL mechanism. Internal-only.
	CertName string

	// PolicyResponse is the verbatim body returned by the policy HTTP
	// endpoint. Internal-only.
	PolicyResponse string
}

// Userdb answers "who is this user?" without a password, for admin
// tooling, master-protocol clients, and the passdb prefetch shortcut.
// Return semantics:
//   - (ui != nil, nil)  — user found, ui populated
//   - (nil, nil)        — user unknown here; UserdbChain tries the next entry
//   - (nil, err != nil) — backend failure; UserdbChain stops and surfaces err
type Userdb interface {
	Lookup(username string) (*UserInfo, error)
}

// UserdbChain composes multiple Userdb backends first-hit-wins. The
// first non-nil response is returned; an error short-circuits the chain.
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

// UserdbIterator lists every known user, for `yarctl backend user
// iterate` and the master-protocol `LIST` command. Optional — a backend
// that cannot enumerate simply does not implement it.
type UserdbIterator interface {
	Iterate() ([]string, error)
}

// VisitFields calls fn for every non-zero typed field in canonical
// order. Internal-only fields (Password, CertName, PolicyResponse) are
// never passed to fn. Lists are comma-joined; booleans emit only when
// true (value "yes"); numbers are decimal. Forward fields emit with the
// `forward_` prefix; Extra emits verbatim. Forward and Extra are
// iterated in lexicographic key order so serialising the same UserInfo
// is byte-identical across callers.
//
// Shared by master.go's marshalUserInfo and protocol.go's
// writeUserdbFields so the field surface stays in lockstep.
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
	str("acl_user", ui.ACLUser)
	list("acl_groups", ui.ACLGroups)

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

	str("director_tag", ui.DirectorTag)

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

// formatUint / formatInt / joinCSV / sortedMapKeys are dependency-free
// helpers shared by VisitFields and the master / client wire serialisers.

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
