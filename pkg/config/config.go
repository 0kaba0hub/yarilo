package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config is the top-level yarilo configuration.
type Config struct {
	Mode                    string                        `koanf:"mode"` // legacy single-binary; ignored by multi-process binaries
	General                 GeneralConfig                 `koanf:"general"`
	Services                ServicesConfig                `koanf:"services"`
	Protocol                ProtocolConfig                `koanf:"protocol"`
	Auth                    AuthConfig                    `koanf:"auth"`
	InternalTLS             InternalTLSConfig             `koanf:"internal_tls"`
	AuthService             AuthServiceConfig             `koanf:"auth_service"`
	AnvilService            AnvilServiceConfig            `koanf:"anvil_service"`
	DirectorService         DirectorServiceConfig         `koanf:"director_service"`
	IMAPLoginService        IMAPLoginServiceConfig        `koanf:"imap_login_service"`
	POP3LoginService        POP3LoginServiceConfig        `koanf:"pop3_login_service"`
	SubmissionLoginSvc      SubmissionLoginServiceConfig  `koanf:"submission_login_service"`
	LMTPLoginService        LMTPLoginServiceConfig        `koanf:"lmtp_login_service"`
	LocksService            LocksServiceConfig            `koanf:"locks_service"`
	LocksClient             LocksClientConfig             `koanf:"locks_client"`
	Storage                 StorageConfig                 `koanf:"storage"`
	Namespaces              []NamespaceConfig             `koanf:"namespaces"`
	Dicts                   map[string]DictConfig         `koanf:"dicts"`
	BackendAPI              BackendAPIConfig              `koanf:"backend_api"`
	QuotaStatus             QuotaStatusConfig             `koanf:"quota_status"`
	SASLLogin               SASLLoginConfig               `koanf:"sasl_login"`
	Sieve                   SieveConfig                   `koanf:"sieve"`
	ManageSieveLoginService ManageSieveLoginServiceConfig `koanf:"managesieve_login_service"`
	Telemetry               TelemetryConfig               `koanf:"telemetry"`
	Log                     LogConfig                     `koanf:"log"`
}

// SieveConfig controls per-user Sieve email filtering (RFC 5228) in the LMTP delivery path.
type SieveConfig struct {
	// Enabled activates Sieve script execution during LMTP delivery.
	Enabled bool `koanf:"enabled"`
	// MaxScriptSize is the maximum compiled script size in bytes. Default: 65536.
	MaxScriptSize int `koanf:"max_script_size"`
	// MaxRedirects is the maximum number of redirect actions per message. Default: 32.
	MaxRedirects int `koanf:"max_redirects"`
	// VacationEnabled permits the vacation extension (RFC 5230). Default: true.
	VacationEnabled bool `koanf:"vacation_enabled"`

	// SubmissionHost is the upstream MTA address (host[:port]) used to send
	// outbound mail for Sieve redirect and vacation actions. Default port 25.
	// Empty string disables outbound sending (redirect/vacation are silently dropped).
	SubmissionHost string `koanf:"submission_host"`
	// SubmissionSSL controls transport security: no | smtps | starttls. Default: no.
	SubmissionSSL string `koanf:"submission_ssl"`
	// SubmissionTimeout is the connect and command timeout in seconds. Default: 30.
	SubmissionTimeout int `koanf:"submission_timeout"`
	// SubmissionAuthSecret is the name of the Kubernetes Secret that holds
	// SMTP AUTH credentials (keys: user, password). Empty = no authentication.
	// The Helm chart mounts the secret as YARILO_SIEVE_SUBMISSION_USER /
	// YARILO_SIEVE_SUBMISSION_PASSWORD env vars based on this name.
	SubmissionAuthSecret string `koanf:"submission_auth_secret"`

	// DefaultName is the reserved name of the per-user default Sieve script
	// (the active-pointer entry point). Corresponds to Dovecot's sieve_default_name.
	// Default: "yarilo".
	DefaultName string `koanf:"default_name"`

	// GlobalBefore is an ordered list of paths to .sieve script files executed
	// before the user's active script. Admin-defined rules; applied to every
	// message regardless of per-user settings.
	GlobalBefore []string `koanf:"global_before"`
	// GlobalAfter is an ordered list of paths to .sieve script files executed
	// after the user's active script.
	GlobalAfter []string `koanf:"global_after"`
}

// DictConfig declares one named dict instance. The map key in
// Config.Dicts is the logical name yarilo features reference (e.g.
// "metadata", "quota_count"); Driver selects the registered
// pkg/dict driver (file|memory|fail|redis|sql) and Settings carries
// the driver-specific knobs. The driver decodes Settings at Open
// time — see each driver's package doc for the schema.
//
// Driver-agnostic settings on top of the per-driver map:
//
//	expire_secs — default TTL for writes; per-op OpSettings overrides
//	username    — passed as OpSettings.Username when callers omit it
//	home_dir    — passed as OpSettings.HomeDir when callers omit it
//
// These three are siblings of "driver" / "settings" in the yaml so
// operators do not have to repeat them in every driver-specific block.
type DictConfig struct {
	Driver     string         `koanf:"driver"`
	Settings   map[string]any `koanf:"settings"`
	ExpireSecs uint32         `koanf:"expire_secs"`
	Username   string         `koanf:"username"`
	HomeDir    string         `koanf:"home_dir"`
}

// NamespaceConfig declares one IMAP namespace (RFC 2342 / RFC 9051
// §6.3.10). The Type field picks which slot of the NAMESPACE response
// the namespace lands in (personal / other / shared). Multiple
// namespaces of the same type are allowed and concatenated in
// declaration order.
//
// Storage routing (NS-1b, future PR): Location templates the on-disk
// path or backend URL for this namespace; the wire-only NS-1a phase
// shipping in v1.20 reads Type, Prefix, Separator and List only — the
// other fields are accepted by koanf so operators can stage their
// full namespace config before the storage layer lands.
//
// Defaults applied at backend startup when cfg.Namespaces is empty:
//
//	[{ Type: "personal", Prefix: "", Separator: "/", List: true }]
//
// — i.e. backwards-compatible with pre-v1.20 single-namespace
// deployments.
type NamespaceConfig struct {
	// Type is one of "personal", "other", "shared". Determines which
	// slot of the IMAP NAMESPACE response carries this entry.
	Type string `koanf:"type"`
	// Prefix is the entry-point name visible to IMAP clients ("",
	// "Shared/", "user/", "Public/", ...). Empty string is reserved
	// for the personal namespace.
	Prefix string `koanf:"prefix"`
	// Separator is the hierarchy delimiter for this namespace.
	// Different namespaces MAY use different separators (matches
	// Dovecot's permissive default).
	Separator string `koanf:"separator"`
	// List exposes the namespace in the NAMESPACE response. False
	// keeps the namespace addressable internally (e.g. for future
	// per-user shared folders) without advertising it.
	List bool `koanf:"list"`
	// Hidden hides matching mailboxes from LIST "" "*" (per-RFC 9051
	// "no-inferiors / no-children" hints). Reserved for NS-1b — koanf
	// accepts it now so config does not have to change later.
	Hidden bool `koanf:"hidden"`
	// Subscriptions: whether SUBSCRIBE state is tracked for mailboxes
	// in this namespace. Default true (per-RFC 5258).
	Subscriptions bool `koanf:"subscriptions"`
	// Inbox marks the namespace that owns the special "INBOX"
	// mailbox. MUST be set on exactly one namespace (typically the
	// first personal namespace). Reserved for NS-1b.
	Inbox bool `koanf:"inbox"`
	// Location is the storage URL for this namespace (NS-1b).
	// Templated via pkg/dict/varexpand (%u, %h, %n, %d). Examples:
	//   "maildir:%h"                       — per-user personal
	//   "maildir:/var/yarilo/shared"       — shared
	//   "maildir:/var/yarilo/public"       — public
	// Read by NS-1b storage routing; NS-1a accepts it for forward-
	// compat but does not act on it.
	Location string `koanf:"location"`
}

// GeneralConfig holds shared infrastructure settings inherited by all services.
type GeneralConfig struct {
	SSL     SSLConfig     `koanf:"ssl"`
	HAProxy HAProxyConfig `koanf:"haproxy"`
	XClient XClientConfig `koanf:"xclient"`
	Limits  LimitsConfig  `koanf:"limits"`
	// StartupDialRetries is the maximum number of dial attempts when connecting
	// to external dependencies (anvil, Redis) at startup. Default 3.
	StartupDialRetries int `koanf:"startup_dial_retries"`
}

type SSLConfig struct {
	TLSCert       string `koanf:"tls_cert"`
	TLSKey        string `koanf:"tls_key"`
	TLSAltCert    string `koanf:"tls_alt_cert"`
	TLSAltKey     string `koanf:"tls_alt_key"`
	TLSMinVersion string `koanf:"tls_min_version"`
	PreferServer  bool   `koanf:"prefer_server_ciphers"`
}

type HAProxyConfig struct {
	Timeout     int      `koanf:"timeout"`      // seconds to wait for PROXY header
	TrustedNets []string `koanf:"trusted_nets"` // CIDRs allowed to send PROXY header
}

type XClientConfig struct {
	TrustedNets []string `koanf:"trusted_nets"` // CIDRs allowed to send XCLIENT
}

type LimitsConfig struct {
	MaxUserIPConnections int `koanf:"mail_max_userip_connections"` // 0 = unlimited
}

// ServiceConfig is per-listener configuration.
// A nil pointer in ServicesConfig means the listener is not started.
type ServiceConfig struct {
	Enabled          bool       `koanf:"enabled"`
	Port             int        `koanf:"port"`
	ConnectionLimit  int        `koanf:"connection_limit"` // 0 = unlimited
	SSLMode          string     `koanf:"ssl_mode"`         // no | ssl | starttls
	SSL              *SSLConfig `koanf:"ssl"`              // overrides general.ssl
	HAProxy          bool       `koanf:"haproxy_protocol"`
	XClient          bool       `koanf:"xclient_protocol"`
	DisablePlainAuth bool       `koanf:"disable_plaintext_auth"`
}

// Active returns true if the service is configured and enabled.
func (s *ServiceConfig) Active() bool { return s != nil && s.Enabled }

// ServicesConfig holds per-listener configuration.
// Nil pointer = listener not started.
type ServicesConfig struct {
	IMAP          *ServiceConfig `koanf:"imap"`           // port 143, STARTTLS
	IMAPS         *ServiceConfig `koanf:"imaps"`          // port 993, SSL
	Submission    *ServiceConfig `koanf:"submission"`     // port 587, STARTTLS outbound
	Submissions   *ServiceConfig `koanf:"submissions"`    // port 465, SSL outbound
	POP3          *ServiceConfig `koanf:"pop3"`           // port 110, STARTTLS
	POP3S         *ServiceConfig `koanf:"pop3s"`          // port 995, SSL
	LMTP          *ServiceConfig `koanf:"lmtp"`           // port 24, local delivery (no auth, loopback only)
	ManageSieve   *ServiceConfig `koanf:"managesieve"`    // port 4190, STARTTLS (login pod)
	ManageSieveBE *ServiceConfig `koanf:"managesieve_be"` // ManageSieve backend (internal)
}

// ProtocolConfig holds protocol-level behaviour settings, independent of listener.
type ProtocolConfig struct {
	IMAP        IMAPProtocolConfig        `koanf:"imap"`
	POP3        POP3ProtocolConfig        `koanf:"pop3"`
	Submission  SubmissionProtocolConfig  `koanf:"submission"`
	LMTP        LMTPProtocolConfig        `koanf:"lmtp"`
	ManageSieve ManageSieveProtocolConfig `koanf:"managesieve"`
}

type LMTPProtocolConfig struct {
	// Greeting shown in the 220 banner. Default: "Yarilo ready."
	LoginGreeting string `koanf:"login_greeting"`
	// AddReceivedHeader prepends a Received: header to delivered messages. Default: true.
	AddReceivedHeader bool `koanf:"add_received_header"`
	// SaveToDetailMailbox delivers user+folder@domain to mailbox 'folder' instead of INBOX. Default: false.
	SaveToDetailMailbox bool `koanf:"save_to_detail_mailbox"`
	// HdrDeliveryAddress controls the Delivered-To header: none | final | original. Default: "final".
	HdrDeliveryAddress string `koanf:"hdr_delivery_address"`
	// VerboseReplies includes diagnostic details in error responses. Default: false.
	VerboseReplies bool `koanf:"verbose_replies"`
	// UserConcurrencyLimit is the max concurrent deliveries per user enforced
	// cluster-wide via yarilo-anvil at RCPT TO. Default: 10 (matches Dovecot).
	// Value 0 is a hard configuration error — operators that genuinely want
	// no limit MUST set -1 ("unlimited"), so a missing or zeroed config can
	// never silently turn off the DoS guard.
	UserConcurrencyLimit int `koanf:"user_concurrency_limit"`
	// ReadTimeout is the per-command read timeout in seconds. Default: 300.
	ReadTimeout int `koanf:"read_timeout"`
	// WriteTimeout is the per-command write timeout in seconds. Default: 300.
	WriteTimeout int `koanf:"write_timeout"`
	// ClientWorkarounds is a list of client compatibility workarounds.
	ClientWorkarounds []string `koanf:"client_workarounds"`
	// Proxy configures LMTP proxy mode (director → backend routing).
	Proxy LMTPProxyConfig `koanf:"proxy"`
	// RateLimit caps deliveries per (sender IP, recipient mailbox)
	// pair within a sliding time window. Defends a specific
	// mailbox from one specific source flooding it. Off by default
	// — opt-in to avoid surprising existing deployments; turn on
	// for any internet-facing LMTP listener.
	RateLimit LMTPRateLimitConfig `koanf:"rate_limit"`
}

// LMTPRateLimitConfig configures the per-(IP, mailbox) token
// bucket enforced at RCPT TO. Counters live in yarilo-locks
// (COUNTER-INC primitive) so the limit is cluster-wide, not
// per-pod.
//
// Defaults are tuned for typical legitimate traffic: 100 RCPT
// for the same (sender IP, recipient mailbox) pair per 60s. A
// regular client sends 1-10 messages per minute even on spike;
// mailing list relays fan-out across recipients (and usually
// across source IPs), so the same (IP, mailbox) pair past 100/min
// is reliably abuse and not legitimate volume.
//
// Operators who run unusual workloads (e.g. a single relay that
// genuinely pumps >100 delivery attempts per minute into one
// recipient) raise the burst, widen the window, or set
// `enabled: false` outright.
type LMTPRateLimitConfig struct {
	// Enabled gates the entire check. Default: true.
	Enabled bool `koanf:"enabled"`
	// PerRecipientBurst is the max deliveries allowed per
	// (sender IP, recipient mailbox) pair inside one window
	// before further RCPT TO commands receive 421 4.7.0.
	// Default: 100.
	PerRecipientBurst int `koanf:"per_recipient_burst"`
	// PerRecipientWindowSeconds is the sliding window width.
	// Default: 60.
	PerRecipientWindowSeconds int `koanf:"per_recipient_window_seconds"`
}

// LMTPProxyConfig holds LMTP proxy settings used on director nodes.
// Backends are taken from the director's ring (general settings); this section
// only controls transport behaviour.
type LMTPProxyConfig struct {
	// Timeout is the per-backend connection+transaction timeout in seconds. Default: 125.
	Timeout int `koanf:"timeout"`
}

type IMAPProtocolConfig struct {
	IdleNotifyInterval int      `koanf:"imap_idle_notify_interval"` // seconds; 0 = disabled
	MaxLineLength      int      `koanf:"imap_max_line_length"`      // bytes; 0 = unlimited
	IDSend             string   `koanf:"imap_id_send"`              // ID pairs; * = default; empty = disabled
	LoginGreeting      string   `koanf:"login_greeting"`
	LogoutFormat       string   `koanf:"imap_logout_format"`
	ClientWorkarounds  []string `koanf:"client_workarounds"`
	// SpecialUseDefaults maps a folder name (case-sensitive) to its RFC 6154
	// special-use attribute. LIST advertises the attr automatically when the
	// folder name matches. Per-user CREATE (USE ...) overrides win against
	// these defaults via the on-disk special_use file. Mirrors Dovecot's
	// namespace.mailbox.special_use convention.
	SpecialUseDefaults map[string]string `koanf:"imap_special_use_defaults"`
	// ACL toggles RFC 4314 server-side ACL: GETACL / SETACL / DELETEACL
	// / MYRIGHTS / LISTRIGHTS. When acl.enabled = false the IMAP server
	// returns NO("ACL extension disabled by operator") on every ACL
	// command. Storage is the per-mailbox `yarilo-acl` file in the
	// folder's index directory.
	ACL ACLConfig `koanf:"acl"`
}

// ACLConfig groups RFC 4314 ACL knobs. PR ACL-1/C ships a single
// enabled flag; richer policy (default ACL, group= resolution, mode
// for negative entries, etc.) lands in later ACL phases.
type ACLConfig struct {
	Enabled bool `koanf:"enabled"`
}

type POP3ProtocolConfig struct {
	NoFlagUpdates  bool   `koanf:"pop3_no_flag_updates"`
	ReuseXUIDL     bool   `koanf:"pop3_reuse_xuidl"`
	UIDLFormat     string `koanf:"pop3_uidl_format"`
	UIDLDuplicates string `koanf:"pop3_uidl_duplicates"` // allow | rename
	EnableLast     bool   `koanf:"pop3_enable_last"`
	DeleteType     string `koanf:"pop3_delete_type"` // expunge | flag
	DeletedFlag    string `koanf:"pop3_deleted_flag"`
	SaveUIDL       bool   `koanf:"pop3_save_uidl"`    // persist computed UIDLs to index
	LockSession    bool   `koanf:"pop3_lock_session"` // dotlock file to prevent IMAP+POP3 conflicts
}

type SubmissionProtocolConfig struct {
	Hostname           string      `koanf:"hostname"`
	MaxMsgSize         int64       `koanf:"max_message_size"`
	MaxLineLength      int         `koanf:"max_line_length"`
	MaxRecipients      int         `koanf:"max_recipients"` // 0 = unlimited (Dovecot default)
	RecipientDelimiter string      `koanf:"recipient_delimiter"`
	Workarounds        []string    `koanf:"client_workarounds"` // whitespace-before-path | mailbox-for-path | implicit-auth-external
	AddReceivedHeader  bool        `koanf:"submission_add_received_header"`
	Relay              RelayConfig `koanf:"relay"`
}

// RelayConfig mirrors Dovecot's submission_relay_* settings.
// Host must be non-empty to enable relaying; otherwise submission returns 451.
type RelayConfig struct {
	Host           string `koanf:"host"`
	Port           int    `koanf:"port"` // default 25
	User           string `koanf:"user"`
	Password       string `koanf:"password"`        // supports ${ENV_VAR}
	SSL            string `koanf:"ssl"`             // no | smtps | starttls
	SSLVerify      bool   `koanf:"ssl_verify"`      // default true
	Trusted        bool   `koanf:"trusted"`         // send XCLIENT to relay (Postfix)
	ConnectTimeout int    `koanf:"connect_timeout"` // seconds, default 30
	CommandTimeout int    `koanf:"command_timeout"` // seconds, default 300
}

// InternalTLSConfig controls mTLS for all inter-component connections.
// When Enabled is false every component listens on plain TCP — use this
// when a service mesh (Istio, Linkerd) handles transport security instead.
type InternalTLSConfig struct {
	Enabled bool   `koanf:"enabled"`
	Cert    string `koanf:"cert"`
	Key     string `koanf:"key"`
	CA      string `koanf:"ca"`
}

// QuotaStatusConfig configures the yarilo-quota-status Postfix policy service.
type QuotaStatusConfig struct {
	// Listen is the TCP address the policy service binds to.
	// Postfix connects here via check_policy_service.
	// Default: ":12340"
	Listen string `koanf:"listen"`
	// DefaultQuotaRules are the site-wide quota limits applied when no
	// per-user rules are available (userdb lookup not yet wired in this phase).
	// Format matches yarilo.yaml quota_rule: ["*:storage=5G", "Trash:storage=+1G"].
	DefaultQuotaRules []string `koanf:"default_quota_rules"`
	// AliasDict is the name of a dict defined in the top-level dicts: map
	// that resolves virtual aliases. The dict key is the recipient address
	// and the returned value is the destination address. Empty = disabled.
	//
	// Example SQL dict query for virtual + catch-all:
	//   SELECT destination FROM virtual_aliases
	//   WHERE source = '%k'
	//      OR source = CONCAT('@', SUBSTRING_INDEX('%k','@',-1))
	//   ORDER BY LENGTH(source) DESC LIMIT 1
	AliasDict string `koanf:"alias_dict"`
	// AliasMaxHops limits alias chain depth to prevent infinite loops.
	// Default: 5
	AliasMaxHops int `koanf:"alias_max_hops"`
	// AuthMasterAddr is the yarilo-auth master-protocol listener address
	// used for per-user userdb lookups (quota_rule fields). When empty,
	// per-user quota rules are disabled and only DefaultQuotaRules apply.
	AuthMasterAddr string `koanf:"auth_master_addr"`
}

// SASLLoginConfig configures the yarilo-sasl-login binary.
// yarilo-sasl-login listens for plain-TCP connections from Postfix (Dovecot
// auth client protocol, smtpd_sasl_type=dovecot) and proxies each session to
// yarilo-auth, optionally wrapping the upstream connection with mTLS.
// This keeps the yarilo-auth socket internal — Postfix has no direct access.
type SASLLoginConfig struct {
	// Listen is the TCP address Postfix connects to.
	// Postfix: smtpd_sasl_path = inet:<host>:<port>
	// Default: ":12325"
	Listen string `koanf:"listen"`
	// AuthAddr is the yarilo-auth client-protocol address to dial.
	// Defaults to auth_service.addr when empty.
	AuthAddr string `koanf:"auth_addr"`
	// TrustedNets lists CIDR ranges allowed to connect.
	// Empty = allow all (not recommended in production).
	TrustedNets []string `koanf:"trusted_nets"`
	// HAProxy enables PROXY protocol v1/v2 header parsing.
	// When true, conn.RemoteAddr() reflects the upstream's real address.
	HAProxy bool `koanf:"haproxy_protocol"`
	// HAProxyTimeout is the read deadline for the PROXY header (seconds).
	HAProxyTimeout int `koanf:"haproxy_timeout"`
	// HAProxyNets lists CIDRs whose PROXY header is trusted.
	// Connections outside these ranges have their PROXY header ignored.
	HAProxyNets []string       `koanf:"haproxy_trusted_nets"`
	Shutdown    ShutdownConfig `koanf:"shutdown"`
}

// AnvilServiceConfig configures the standalone yarilo-anvil process.
type AnvilServiceConfig struct {
	Listen string `koanf:"listen"`
	// Addr is the address login pods use to dial yarilo-anvil.
	// Defaults to Listen when empty (single-process / dev mode).
	// In k8s set to the ClusterIP service DNS, e.g. "yarilo-anvil:9101".
	Addr     string         `koanf:"addr"`
	Shutdown ShutdownConfig `koanf:"shutdown"`
	// FailOpen controls login-pod behaviour when yarilo-anvil is unreachable.
	// true = allow the session; false (default) = reject the session.
	FailOpen bool `koanf:"fail_open"`
}

// ClientAddr returns the address login pods use to dial yarilo-anvil.
func (c AnvilServiceConfig) ClientAddr() string {
	if c.Addr != "" {
		return c.Addr
	}
	return c.Listen
}

// AuthServiceConfig configures the standalone yarilo-auth process.
//
// Listen is the client-protocol address (login pods speak this);
// MasterListen, when non-empty, opens the password-less master
// protocol on a separate listener for admin tooling and
// yarilo-backend-api userdb lookups. Both listeners share the
// global InternalTLS material — splitting trust domains is a
// future operational knob, not in Phase AUTH-1.
type AuthServiceConfig struct {
	Listen string `koanf:"listen"`
	// Addr is the address login pods use to dial yarilo-auth.
	// Defaults to Listen when empty (single-process / dev mode).
	// In k8s set to the ClusterIP service DNS, e.g. "yarilo-auth:9100".
	Addr         string `koanf:"addr"`
	MasterListen string `koanf:"master_listen"`
	// MasterAddr is the address backend services use to dial the
	// yarilo-auth master protocol for userdb lookups (USER command).
	// Defaults to empty (userdb checks disabled) when not set.
	MasterAddr string         `koanf:"master_addr"`
	Shutdown   ShutdownConfig `koanf:"shutdown"`
}

// ClientAddr returns the address login pods use to dial yarilo-auth.
func (c AuthServiceConfig) ClientAddr() string {
	if c.Addr != "" {
		return c.Addr
	}
	return c.Listen
}

// LocksClientConfig configures how session processes (yarilo-imap,
// yarilo-pop3, yarilo-submission, yarilo-lmtp) connect to a yarilo-locks
// service. Empty Mode disables cross-process locking — single-process tests
// and CLI dev runs. Production k8s sets Mode=remote with one or more
// Endpoints pointing at the yarilo-locks ClusterIP Service.
type LocksClientConfig struct {
	Mode      string   `koanf:"mode"`      // remote | embedded | ""
	Endpoints []string `koanf:"endpoints"` // remote: ["yarilo-locks.svc:9104", ...]
	Socket    string   `koanf:"socket"`    // embedded: /run/yarilo/locks.sock
}

// LocksServiceConfig configures the standalone yarilo-locks process.
// Mode "embedded" runs an in-memory server on a Unix socket (standalone deployment).
// Mode "remote" runs a Redis-backed server on TCP+mTLS (backend deployment per tag).
// Empty Mode disables the locks server in this process.
type LocksServiceConfig struct {
	Mode          string         `koanf:"mode"`           // embedded | remote | ""
	Socket        string         `koanf:"socket"`         // embedded: /run/yarilo/locks.sock
	Listen        string         `koanf:"listen"`         // remote: ":9104"
	Redis         string         `koanf:"redis"`          // remote: "redis://host:6379/0"
	KeyPrefix     string         `koanf:"key_prefix"`     // remote: default "yarilo:locks:"
	ChannelPrefix string         `koanf:"channel_prefix"` // remote: default "yarilo:events:"
	Shutdown      ShutdownConfig `koanf:"shutdown"`
}

// MailServerConfig describes one backend mail server the director routes sessions to.
type MailServerConfig struct {
	Host string `koanf:"host"`
	Port int    `koanf:"port"`
	// Tag groups backends into pools; an empty tag means the default pool.
	Tag string `koanf:"tag"`
}

// DirectorAPIConfig configures the HTTP admin API on yarilo-director.
type DirectorAPIConfig struct {
	Listen      string   `koanf:"listen"`       // default ":9103"
	Token       string   `koanf:"token"`        // Bearer token; supports ${ENV_VAR}
	AllowedNets []string `koanf:"allowed_nets"` // CIDRs allowed to call the API
}

// DirectorServiceConfig configures the standalone yarilo-director process.
type DirectorServiceConfig struct {
	Listen       string             `koanf:"listen"`
	Shutdown     ShutdownConfig     `koanf:"shutdown"`
	UserExpire   int                `koanf:"user_expire"`   // seconds before user→backend mapping expires; 0 = 900
	PingInterval int                `koanf:"ping_interval"` // seconds between PING probes; 0 = 30
	PingTimeout  int                `koanf:"ping_timeout"`  // seconds to wait for PONG before closing; 0 = 10
	MailServers  []MailServerConfig `koanf:"mail_servers"`  // static backend list, loaded at startup
	Peers        []string           `koanf:"peers"`         // peer director addresses "host:port" for ring sync (replicas > 1)
	API          DirectorAPIConfig  `koanf:"api"`
}

// IMAPLoginServiceConfig configures the yarilo-imap-login proxy.
// BackendAddr, when set, bypasses director LOOKUP and routes every
// session directly to this address (standalone k8s deployments).
// Leave empty in director deployments.
type IMAPLoginServiceConfig struct {
	BackendAddr string `koanf:"backend_addr"`
}

// POP3LoginServiceConfig mirrors IMAPLoginServiceConfig for the POP3 proxy.
type POP3LoginServiceConfig struct {
	BackendAddr string `koanf:"backend_addr"`
}

// SubmissionLoginServiceConfig mirrors IMAPLoginServiceConfig for the Submission proxy.
type SubmissionLoginServiceConfig struct {
	BackendAddr string `koanf:"backend_addr"`
}

// ManageSieveLoginServiceConfig configures the yarilo-managesieve-login proxy (RFC 5804).
type ManageSieveLoginServiceConfig struct {
	// BackendAddr is the fixed address of the yarilo-managesieve backend.
	BackendAddr string `koanf:"backend_addr"`
	// HAProxy enables PROXY protocol v1/v2 header parsing.
	HAProxy bool `koanf:"haproxy_protocol"`
	// HAProxyTimeout is the read deadline for the PROXY header in seconds.
	HAProxyTimeout int `koanf:"haproxy_timeout"`
	// HAProxyNets lists CIDRs whose PROXY header is trusted.
	HAProxyNets []string `koanf:"haproxy_trusted_nets"`
}

// ManageSieveProtocolConfig holds ManageSieve protocol-level behaviour settings.
type ManageSieveProtocolConfig struct {
	// MaxScriptSize is the maximum size in bytes of a single Sieve script.
	// Default: 65536.
	MaxScriptSize int `koanf:"max_script_size"`
	// MaxInvalidCommands is the number of unrecognised pre-auth commands
	// after which the server sends BYE and closes the connection. Default: 3.
	MaxInvalidCommands int `koanf:"max_invalid_commands"`
}

// LMTPLoginServiceConfig configures the yarilo-lmtp-login proxy.
type LMTPLoginServiceConfig struct {
	// BackendAddr is used in standalone mode: fixed address of the yarilo-lmtp
	// backend. Ignored when DirectorAddr is set.
	BackendAddr string `koanf:"backend_addr"`

	// DirectorAddr enables director mode: per-recipient LOOKUP via yarilo-director.
	// When set, BackendAddr is ignored and each RCPT TO triggers a LOOKUP request.
	DirectorAddr string `koanf:"director_addr"`
	// DirectorTag restricts LOOKUP to backends carrying this tag. Empty = full ring.
	DirectorTag string `koanf:"director_tag"`
	// BackendPort overrides the port returned by a director LOOKUP. 0 = use as-is.
	BackendPort int `koanf:"backend_port"`
}

// BackendAPIConfig configures the yarilo-backend-api process —
// the backend-plane HTTP admin surface (dict / acl / quota /
// folder / user / mailbox / ...). One instance runs per backend
// tag (or one per standalone deployment, where it serves the
// single combined session pod).
//
// The director-plane HTTP admin (ring / backends / users / peers)
// is hosted by yarilo-director on its own port; the yarilo-admin
// CLI surfaces both through nested subcommands (`yarilo-admin
// director ...` vs `yarilo-admin backend ...`).
type BackendAPIConfig struct {
	Listen      string   `koanf:"listen"`       // ":9105" default
	Token       string   `koanf:"token"`        // Bearer token; supports ${ENV_VAR} via koanf
	AllowedNets []string `koanf:"allowed_nets"` // CIDRs allowed to call the API

	// AuthMasterAddr is the yarilo-auth master-protocol listener
	// (typically the same `yarilo-auth.<release>:9102` the
	// auth_service.master_listen config exposes). When non-empty,
	// yarilo-backend-api dials it at startup via pkg/authclient and
	// enriches `/api/backend/user/info` with userdb fields plus
	// serves `/api/backend/user/iterate`. Empty disables both
	// surfaces — useful for dev / smoke runs that have no
	// yarilo-auth instance to talk to.
	AuthMasterAddr string `koanf:"auth_master_addr"`
}

// ShutdownConfig controls graceful shutdown behaviour.
type ShutdownConfig struct {
	SessionGracePeriod int `koanf:"session_grace_period"` // seconds to drain sessions before exit
	KillTimeout        int `koanf:"kill_timeout"`         // seconds after grace before SIGKILL
}

type AuthConfig struct {
	Passdb []PassdbEntry `koanf:"passdb"`

	// MasterUsers groups master-user impersonation
	// settings. Disabled by default — distinct SASL PLAIN authzid
	// is rejected by every protocol entry point (IMAP, POP3,
	// Submission, wire yarilo-auth) until Enabled is flipped to
	// true. When disabled, all other fields in this group are
	// ignored even if populated.
	MasterUsers MasterUsersConfig `koanf:"master_users"`

	// MaxAttempts is the number of authentication attempts a client may
	// make on a single connection before the server sends BYE / -ERR and
	// closes. Applies to IMAP and POP3; SMTP submission always closes after
	// the first failure. Matches Dovecot auth_max_attempts. Default 3.
	MaxAttempts int `koanf:"max_attempts"`

	// FailureDelaySeconds is the timing-leak mitigation: every
	// failed auth reply (wrong password, unknown user, malformed
	// SASL response) is held back this many seconds before
	// surfacing to the client. The delay equalises response time
	// across success / fail / unknown-user code paths so an
	// attacker cannot use timing to enumerate users or distinguish
	// "user exists, wrong password" from "user does not exist".
	//
	// Default 2 (seconds). Set to 0 to disable (mainly for test
	// speed — production should always have it >0).
	FailureDelaySeconds int `koanf:"failure_delay"`

	// InternalFailureDelayMs is the matching delay for INTERNAL
	// failures — passdb backend down, SQL connection refused, etc.
	// Separate knob because internal failures often retry and the
	// operator may want a shorter back-off than the user-facing
	// FailureDelay. Default 2000ms.
	InternalFailureDelayMs int `koanf:"internal_failure_delay_ms"`

	// Cache groups passdb / userdb cache settings. Disabled by
	// default (SizeBytes=0).
	Cache AuthCacheConfig `koanf:"cache"`

	// Penalty groups the cross-pod IP-bound auth-fail backoff.
	// Opt-in: Enabled=false skips both Lookup and Update entirely
	// (every auth runs at full speed regardless of prior fails).
	// When enabled, requires the anvil service to be reachable.
	Penalty AuthPenaltyConfig `koanf:"penalty"`

	// Token configures the one-time session token issued by yarilo-auth
	// after a successful passdb check and consumed by the backend to
	// enter authenticated state without re-running passdb.
	Token AuthTokenConfig `koanf:"token"`

	// Policy groups the external HTTP policy-server hook (wforce-
	// compatible). URL="" disables.
	Policy AuthPolicyConfig `koanf:"policy"`

	// OAuth2 is the list of configured OAuth providers. Each entry
	// builds one passdb that participates in the chain alongside
	// the SQL passdbs in Passdb. Empty list disables the
	// OAUTHBEARER SASL mechanism.
	OAuth2 []OAuth2Entry `koanf:"oauth2"`
}

// OAuth2Mode picks the validation transport.
type OAuth2Mode string

const (
	// OAuth2ModeLocalJWT verifies the bearer token's signature
	// locally against a cached JWKS.
	OAuth2ModeLocalJWT OAuth2Mode = "local"
	// OAuth2ModeIntrospection calls an RFC 7662 introspection
	// endpoint. Transport sub-mode picks the request shape.
	OAuth2ModeIntrospection OAuth2Mode = "introspection"
	// OAuth2ModeTokeninfo calls a Google-style tokeninfo endpoint.
	OAuth2ModeTokeninfo OAuth2Mode = "tokeninfo"
	// OAuth2ModeDiscovery auto-resolves jwks_uri /
	// introspection_endpoint from the IdP's OIDC discovery
	// document at `<issuer>/.well-known/openid-configuration`.
	OAuth2ModeDiscovery OAuth2Mode = "discovery"
)

// OAuth2Entry configures one OAuth provider. The required fields
// depend on Mode:
//
//   - local         → JWKSURL
//   - introspection → IntrospectionURL (+ ClientID/ClientSecret)
//   - tokeninfo     → TokeninfoURL
//   - discovery     → IssuerURL
//
// Username / Issuers / Audience / Scope / Active / Grace fields
// apply across modes.
type OAuth2Entry struct {
	// Mode picks the validation transport. REQUIRED.
	Mode OAuth2Mode `koanf:"mode"`

	// Endpoints — one of these must be set, per Mode.
	JWKSURL          string `koanf:"jwks_url"`
	IntrospectionURL string `koanf:"introspection_url"`
	TokeninfoURL     string `koanf:"tokeninfo_url"`
	IssuerURL        string `koanf:"issuer_url"`

	// IntrospectionMode controls the introspection request shape.
	// One of "post" (default — RFC 7662), "auth", "get".
	IntrospectionMode string `koanf:"introspection_mode"`

	// PreferIntrospection (discovery mode only) — when true,
	// prefers the introspection endpoint over JWKS when both are
	// advertised in the discovery document.
	PreferIntrospection bool `koanf:"prefer_introspection"`

	// ClientID / ClientSecret authenticate the introspection call
	// itself. Ignored in JWKS and tokeninfo modes.
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`

	// Issuers is the allow-list of `iss` claim values. Empty =
	// no check (any signed token from a key in JWKS passes).
	// In discovery mode the document's iss is auto-added.
	Issuers []string `koanf:"issuers"`

	// Audience is the required `aud` claim. Empty = no check.
	Audience string `koanf:"audience"`

	// Scopes lists scopes every token MUST carry. Empty = no
	// check.
	Scopes []string `koanf:"scopes"`

	// UsernameAttribute is the response/claim name resolving to
	// the mail user. Default "email".
	UsernameAttribute string `koanf:"username_attribute"`

	// UsernameValidationFormat is the template applied to the
	// SASL authzid before comparing to the username claim.
	// Default "%{user}" (identity). Supports %u, %{user}, %Lu,
	// %n, %Ln, %d, %Ld.
	UsernameValidationFormat string `koanf:"username_validation_format"`

	// ActiveAttribute / ActiveValue — optional check. The claim
	// named by ActiveAttribute must be present (and equal
	// ActiveValue if non-empty) for the login to succeed.
	ActiveAttribute string `koanf:"active_attribute"`
	ActiveValue     string `koanf:"active_value"`

	// ExtraFields are the claim names that get projected back
	// onto the auth response as userdb_<claim> fields.
	ExtraFields []string `koanf:"extra_fields"`

	// TokenExpireGraceSeconds tolerates clock skew. Default 60.
	TokenExpireGraceSeconds int `koanf:"token_expire_grace_seconds"`

	// HTTPTimeoutMs caps the validation round-trip (introspection
	// / tokeninfo / discovery / JWKS-refresh). Default 5000.
	HTTPTimeoutMs int `koanf:"http_timeout_ms"`
}

// AuthPenaltyConfig configures cross-pod IP-bound auth backoff.
// State lives in the anvil service so a single attacker IP pays
// the exponential cost across every auth pod they land on.
type AuthPenaltyConfig struct {
	// Enabled toggles the feature. Default false.
	Enabled bool `koanf:"enabled"`
}

// AuthTokenConfig configures the one-time session token that yarilo-auth
// issues after a successful passdb and that the backend consumes via VERIFY
// to enter authenticated state without replaying credentials.
type AuthTokenConfig struct {
	// TTLSeconds is how long the token remains valid for the backend to
	// consume. Default 60 s — enough for the login pod to forward it and
	// the backend to call VERIFY within the same connection setup.
	TTLSeconds int `koanf:"ttl_seconds"`
	// Backend selects the token store implementation: "memory" (default,
	// single-pod only) or "redis" (multi-replica safe).
	Backend string `koanf:"backend"`
	// RedisAddr is a Redis URL used when Backend="redis".
	// Format: redis://[password@]host:port/db
	RedisAddr string `koanf:"redis_addr"`
}

// AuthPolicyConfig configures the external HTTP policy-server
// hook. URL="" disables the feature.
type AuthPolicyConfig struct {
	// URL is the policy endpoint. Empty disables the hook.
	URL string `koanf:"url"`

	// APIHeader is added to every request. Two formats:
	//   "Key: value"  → custom header
	//   "value"       → X-API-Key: value
	APIHeader string `koanf:"api_header"`

	// HashMech is the digest for the pwhash field. "sha256"
	// (default) or "sha512". Must match the policy server's
	// configured hash.
	HashMech string `koanf:"hash_mech"`

	// HashNonce is the per-deployment salt. REQUIRED when URL is
	// set; empty nonce is rejected at startup so two deployments
	// don't share pwhash space.
	HashNonce string `koanf:"hash_nonce"`

	// HashTruncateBits caps the MSB bits of the digest. Default
	// 12 (4096 buckets — useful for rate-limit patterns, useless
	// for password recovery). 0 means no truncation.
	HashTruncateBits uint `koanf:"hash_truncate_bits"`

	// TimeoutMs is the HTTP round-trip cap. Default 5000ms.
	TimeoutMs int `koanf:"timeout_ms"`

	// RejectOnFail flips fail-open to fail-closed: when true and
	// the policy server is unreachable / returns malformed JSON,
	// the auth attempt is rejected. Default false (fail-open).
	RejectOnFail bool `koanf:"reject_on_fail"`

	// LogOnly: when true, the client still calls the server and
	// logs decisions, but the decision is NOT enforced — used
	// for shadow-mode rollout before flipping the switch.
	LogOnly bool `koanf:"log_only"`

	// CheckBefore: POST ?command=allow BEFORE the chain runs.
	// Default true.
	CheckBefore bool `koanf:"check_before"`

	// CheckAfter: POST ?command=allow AFTER the chain result is
	// known. Default true.
	CheckAfter bool `koanf:"check_after"`

	// ReportAfter: POST ?command=report fire-and-forget after
	// every decision. Default true.
	ReportAfter bool `koanf:"report_after"`
}

// AuthCacheConfig configures the in-process auth cache (LRU
// bytes-bounded). Positive entries hold successful credentials
// verified against the stored HMAC of the password; negative
// entries hold failed lookups (unknown user, wrong password).
// Set SizeBytes>0 to enable.
type AuthCacheConfig struct {
	// SizeBytes caps total payload weight (approximate — includes
	// key + bag + per-entry overhead). 0 disables caching.
	SizeBytes int64 `koanf:"size_bytes"`

	// TTLSeconds is the lifetime of a positive (successful) entry.
	// Default 1800 (30m). Kept tight because yarilo lacks
	// automatic flush hooks from user-management tooling yet
	// (see AUTH-7 backlog), so a shorter window limits staleness
	// exposure for password rotation / user deletion.
	TTLSeconds int `koanf:"ttl_seconds"`

	// NegativeTTLSeconds is the lifetime of a negative (failed)
	// entry. Default 1800 (30m). Cache poisoning by wrong-password
	// retries is already prevented at the cache layer (see
	// Cache.Insert anti-poisoning guard); this TTL governs only
	// genuinely-unknown-user entries.
	NegativeTTLSeconds int `koanf:"negative_ttl_seconds"`
}

// MasterUsersConfig configures the master-user impersonation
// surface. Master-user lets a privileged account log into another
// user's mailbox by sending a SASL PLAIN response with the
// target's identity in authzid. See INTERNALS.md for the wire
// model.
type MasterUsersConfig struct {
	// Enabled is the top-level opt-in. While false, distinct
	// SASL PLAIN authzid is rejected unconditionally — the wire
	// reply is indistinguishable from a wrong-password rejection.
	// Default: false.
	Enabled bool `koanf:"enabled"`

	// Masterdb is the dedicated master-user chain. Entries share
	// the PassdbEntry shape (same SQL drivers, same query syntax)
	// but are only consulted when a SASL PLAIN client sends a
	// distinct authzid — i.e. requests to impersonate another user.
	// A masterdb hit (correct master password) grants impersonation;
	// the regular Passdb chain then runs against the TARGET to
	// fetch their profile.
	//
	// Independently of Masterdb, any regular Passdb entry can flag
	// individual rows as masters by returning `master_user=yes` in
	// the result fields. Both mechanisms coexist.
	Masterdb []PassdbEntry `koanf:"masterdb"`

	// Separator enables the `target<sep>master` SASL PLAIN
	// workaround for legacy clients that cannot supply authzid
	// (older Outlook, some mobile MUAs). The SASL response
	// authid `alice*admin` then routes as if authzid had been
	// `alice` and authid had been `admin`. Empty disables the
	// workaround entirely — only RFC 4616 authzid is honoured.
	// Default `*` — only takes effect when Enabled is true.
	Separator string `koanf:"separator"`
}

type PassdbEntry struct {
	Driver            string `koanf:"driver"` // sqlite | mysql | postgres
	DSN               string `koanf:"dsn"`
	PasswordQuery     string `koanf:"password_query"`      // custom SELECT; %u/%n/%d substituted as parameters
	UserQuery         string `koanf:"user_query"`          // optional userdb lookup; %u/%n/%d substituted
	IterateQuery      string `koanf:"iterate_query"`       // optional list-users query (admin tooling)
	DefaultPassScheme string `koanf:"default_pass_scheme"` // assumed scheme when stored password has no {SCHEME} prefix (default PLAIN)
	SkipSchema        bool   `koanf:"skip_schema"`         // do not run CREATE TABLE IF NOT EXISTS on startup
}

type StorageConfig struct {
	Mailbox          string `koanf:"mailbox"`
	MaildirRoot      string `koanf:"maildir_root"`
	MailHomeTemplate string `koanf:"mail_home_template"`
	Index            string `koanf:"index"`
	IndexDir         string `koanf:"index_dir"`
	// MdboxAltStoragePath is the base directory for the mdbox alt
	// (cold) storage tier. Supports the same %u/%n/%d/%Lu/%Ln/%Ld
	// template variables as mail_home_template. Empty disables alt
	// storage (default). Mirrors Dovecot's mail_alt_path setting.
	// Example: /mnt/cold/%d/%n
	MdboxAltStoragePath string `koanf:"mdbox_alt_storage_path"`

	// VolatileDir is the cluster-wide VOLATILEDIR template. When set,
	// the fileindex Recreate tmp file is written here (typically a
	// local tmpfs) and then copied to NFS, keeping the expensive fsync
	// off the NFS path. Supports %u/%n/%d/%h template variables.
	// Mirrors Dovecot's VOLATILEDIR mail-location modifier.
	// Example: /run/yarilo-volatile/%d/%n
	VolatileDir string `koanf:"volatile_dir"`

	// ControlDir is the cluster-wide CONTROL= template. When set,
	// per-folder control files (yarilo-uidlist, subscriptions) are
	// stored here instead of co-located with the mailbox data under
	// home. Supports %u/%n/%d/%h template variables.
	// Mirrors Dovecot's CONTROL= mail-location modifier.
	// Example: /var/yarilo-control/%d/%n
	ControlDir string `koanf:"control_dir"`

	// AltDir is the cluster-wide ALT= template. When set, enables
	// two-tier maildir storage: messages cold-tiered via altmove
	// live here; reads check both primary (home) and alt tiers.
	// Supports %u/%n/%d/%h template variables.
	// Mirrors Dovecot's ALT= mail-location modifier.
	// Example: /mnt/cold/%d/%n
	AltDir string `koanf:"alt_dir"`
}

type TelemetryConfig struct {
	Listen string `koanf:"listen"`
}

type LogConfig struct {
	Level string `koanf:"level"`
}

// Load reads the YAML config file at path and applies defaults.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, err
	}
	defaultTrustedNets := []string{"127.0.0.1/32", "10.0.0.0/8"}
	cfg := &Config{
		Mode: "single",
		General: GeneralConfig{
			SSL: SSLConfig{TLSMinVersion: "TLS1.2"},
			HAProxy: HAProxyConfig{
				Timeout:     3,
				TrustedNets: defaultTrustedNets,
			},
			XClient:            XClientConfig{TrustedNets: defaultTrustedNets},
			Limits:             LimitsConfig{MaxUserIPConnections: 10},
			StartupDialRetries: 3,
		},
		Protocol: ProtocolConfig{
			IMAP: IMAPProtocolConfig{
				IdleNotifyInterval: 120,
				MaxLineLength:      65536,
				IDSend:             "name *",
				// Dovecot-compat conventional special-use mappings.
				// Operators override via yarilo.yaml; per-user CREATE USE
				// overrides via the on-disk special_use file.
				SpecialUseDefaults: map[string]string{
					"Sent":    `\Sent`,
					"Drafts":  `\Drafts`,
					"Trash":   `\Trash`,
					"Junk":    `\Junk`,
					"Archive": `\Archive`,
				},
			},
			POP3: POP3ProtocolConfig{
				UIDLFormat:     "%u.%v",
				UIDLDuplicates: "rename",
				DeleteType:     "expunge",
			},
			Submission: SubmissionProtocolConfig{
				MaxMsgSize:         41943040,
				MaxLineLength:      4096,
				RecipientDelimiter: "+",
				AddReceivedHeader:  true,
				Relay: RelayConfig{
					Port:           25,
					SSL:            "no",
					SSLVerify:      true,
					ConnectTimeout: 30,
					CommandTimeout: 300,
				},
			},
			LMTP: LMTPProtocolConfig{
				LoginGreeting:        "Yarilo ready.",
				AddReceivedHeader:    true,
				HdrDeliveryAddress:   "final",
				ReadTimeout:          300,
				WriteTimeout:         300,
				UserConcurrencyLimit: 10,
				RateLimit: LMTPRateLimitConfig{
					Enabled:                   true,
					PerRecipientBurst:         100,
					PerRecipientWindowSeconds: 60,
				},
			},
		},
		InternalTLS: InternalTLSConfig{
			Enabled: true,
			Cert:    "/etc/yarilo/tls/tls.crt",
			Key:     "/etc/yarilo/tls/tls.key",
			CA:      "/etc/yarilo/tls/ca.crt",
		},
		AnvilService: AnvilServiceConfig{
			Listen: ":9101",
			Shutdown: ShutdownConfig{
				SessionGracePeriod: 30,
				KillTimeout:        5,
			},
		},
		AuthService: AuthServiceConfig{
			Listen: ":9100",
			Shutdown: ShutdownConfig{
				SessionGracePeriod: 30,
				KillTimeout:        5,
			},
		},
		Auth: AuthConfig{
			// MasterUsers is opt-in (Enabled defaults to false).
			// Separator stays inert until Enabled is flipped
			// explicitly.
			MasterUsers: MasterUsersConfig{
				Separator: "*",
			},
			MaxAttempts: 3,
			// 2s for client-visible failures, 2000ms for internal.
			FailureDelaySeconds:    2,
			InternalFailureDelayMs: 2000,
			// Cache off by default — operators opt in by setting
			// auth.cache.size_bytes>0. TTLs are 30m to keep
			// password-change / user-delete staleness windows
			// tight in environments without explicit cache
			// flushes from user-management tooling.
			Cache: AuthCacheConfig{
				TTLSeconds:         1800,
				NegativeTTLSeconds: 1800,
			},
			Token: AuthTokenConfig{
				TTLSeconds: 60,
				Backend:    "memory",
			},
			Policy: AuthPolicyConfig{
				HashMech:         "sha256",
				HashTruncateBits: 12,
				TimeoutMs:        5000,
				CheckBefore:      true,
				CheckAfter:       true,
				ReportAfter:      true,
			},
		},
		DirectorService: DirectorServiceConfig{
			Listen: ":9102",
			Shutdown: ShutdownConfig{
				SessionGracePeriod: 30,
				KillTimeout:        5,
			},
			UserExpire:   900,
			PingInterval: 30,
			PingTimeout:  10,
			API: DirectorAPIConfig{
				Listen: ":9103",
				// 127.0.0.0/8   — loopback (same-pod CLI)
				// 10.96.0.0/12  — k8s service CIDR (kubeadm default)
				// 10.244.0.0/16 — k8s pod CIDR (flannel/kubeadm default)
				AllowedNets: []string{"127.0.0.0/8", "10.96.0.0/12", "10.244.0.0/16"},
			},
		},
		QuotaStatus: QuotaStatusConfig{Listen: ":12340"},
		SASLLogin: SASLLoginConfig{
			Listen:         ":12325",
			HAProxyTimeout: 3,
		},
		Telemetry: TelemetryConfig{Listen: ":8080"},
		Log:       LogConfig{Level: "info"},
		Sieve: SieveConfig{
			DefaultName:       "yarilo",
			MaxScriptSize:     65536,
			MaxRedirects:      32,
			VacationEnabled:   true,
			SubmissionSSL:     "no",
			SubmissionTimeout: 30,
		},
	}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	expandEnv(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (cfg *Config) validate() error {
	if cfg.InternalTLS.Enabled {
		if cfg.InternalTLS.Cert == "" || cfg.InternalTLS.Key == "" || cfg.InternalTLS.CA == "" {
			return fmt.Errorf("config: internal_tls.enabled is true but cert/key/ca are not set")
		}
	}
	// Mirror Dovecot lmtp-settings.c:215 — a 0 value silently disables the
	// per-user concurrency guard, which is a multi-tenant footgun. Force
	// operators to pick: leave default (10), set a positive integer, or
	// -1 for "unlimited".
	if cfg.Services.LMTP.Active() && cfg.Protocol.LMTP.UserConcurrencyLimit == 0 {
		return fmt.Errorf(`config: lmtp.user_concurrency_limit must not be 0 (did you mean "unlimited" via -1?)`)
	}
	return nil
}

// ResolveSSL merges general.ssl with a per-service SSL override (service wins per field).
func (cfg *Config) ResolveSSL(svc *ServiceConfig) SSLConfig {
	merged := cfg.General.SSL
	if svc.SSL == nil {
		return merged
	}
	if svc.SSL.TLSCert != "" {
		merged.TLSCert = svc.SSL.TLSCert
	}
	if svc.SSL.TLSKey != "" {
		merged.TLSKey = svc.SSL.TLSKey
	}
	if svc.SSL.TLSAltCert != "" {
		merged.TLSAltCert = svc.SSL.TLSAltCert
	}
	if svc.SSL.TLSAltKey != "" {
		merged.TLSAltKey = svc.SSL.TLSAltKey
	}
	if svc.SSL.TLSMinVersion != "" {
		merged.TLSMinVersion = svc.SSL.TLSMinVersion
	}
	return merged
}

// BuildTLSConfig constructs a *tls.Config from an SSLConfig.
func BuildTLSConfig(ssl SSLConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(ssl.TLSCert, ssl.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("tls: load cert %q: %w", ssl.TLSCert, err)
	}
	certs := []tls.Certificate{cert}
	if ssl.TLSAltCert != "" && ssl.TLSAltKey != "" {
		alt, err := tls.LoadX509KeyPair(ssl.TLSAltCert, ssl.TLSAltKey)
		if err != nil {
			return nil, fmt.Errorf("tls: load alt cert %q: %w", ssl.TLSAltCert, err)
		}
		certs = append(certs, alt)
	}
	tlsCfg := &tls.Config{
		Certificates:             certs,
		PreferServerCipherSuites: ssl.PreferServer,
		MinVersion:               tls.VersionTLS12,
	}
	if ssl.TLSMinVersion == "TLS1.3" {
		tlsCfg.MinVersion = tls.VersionTLS13
	}
	return tlsCfg, nil
}

func expandEnv(cfg *Config) {
	cfg.General.SSL.TLSCert = expand(cfg.General.SSL.TLSCert)
	cfg.General.SSL.TLSKey = expand(cfg.General.SSL.TLSKey)
	cfg.General.SSL.TLSAltCert = expand(cfg.General.SSL.TLSAltCert)
	cfg.General.SSL.TLSAltKey = expand(cfg.General.SSL.TLSAltKey)
	cfg.InternalTLS.Cert = expand(cfg.InternalTLS.Cert)
	cfg.InternalTLS.Key = expand(cfg.InternalTLS.Key)
	cfg.InternalTLS.CA = expand(cfg.InternalTLS.CA)
	expandSvcSSL(cfg.Services.IMAP)
	expandSvcSSL(cfg.Services.IMAPS)
	expandSvcSSL(cfg.Services.Submission)
	expandSvcSSL(cfg.Services.Submissions)
	expandSvcSSL(cfg.Services.POP3)
	expandSvcSSL(cfg.Services.POP3S)
	cfg.DirectorService.API.Token = expand(cfg.DirectorService.API.Token)
	cfg.Protocol.Submission.Relay.Password = expand(cfg.Protocol.Submission.Relay.Password)
	for i := range cfg.Auth.Passdb {
		cfg.Auth.Passdb[i].DSN = expand(cfg.Auth.Passdb[i].DSN)
	}
	for i := range cfg.Auth.MasterUsers.Masterdb {
		cfg.Auth.MasterUsers.Masterdb[i].DSN = expand(cfg.Auth.MasterUsers.Masterdb[i].DSN)
	}
}

func expandSvcSSL(svc *ServiceConfig) {
	if svc == nil || svc.SSL == nil {
		return
	}
	svc.SSL.TLSCert = expand(svc.SSL.TLSCert)
	svc.SSL.TLSKey = expand(svc.SSL.TLSKey)
	svc.SSL.TLSAltCert = expand(svc.SSL.TLSAltCert)
	svc.SSL.TLSAltKey = expand(svc.SSL.TLSAltKey)
}

func expand(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return os.ExpandEnv(s)
}
