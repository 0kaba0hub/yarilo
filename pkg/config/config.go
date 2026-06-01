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
	Mode            string                `koanf:"mode"` // legacy single-binary; ignored by multi-process binaries
	General         GeneralConfig         `koanf:"general"`
	Services        ServicesConfig        `koanf:"services"`
	Protocol        ProtocolConfig        `koanf:"protocol"`
	Auth            AuthConfig            `koanf:"auth"`
	InternalTLS     InternalTLSConfig     `koanf:"internal_tls"`
	AuthService     AuthServiceConfig     `koanf:"auth_service"`
	AnvilService    AnvilServiceConfig    `koanf:"anvil_service"`
	DirectorService DirectorServiceConfig `koanf:"director_service"`
	LocksService    LocksServiceConfig    `koanf:"locks_service"`
	LocksClient     LocksClientConfig     `koanf:"locks_client"`
	Storage         StorageConfig         `koanf:"storage"`
	Namespaces      []NamespaceConfig     `koanf:"namespaces"`
	Dicts           map[string]DictConfig `koanf:"dicts"`
	BackendAPI      BackendAPIConfig      `koanf:"backend_api"`
	Telemetry       TelemetryConfig       `koanf:"telemetry"`
	Log             LogConfig             `koanf:"log"`
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
	IMAP        *ServiceConfig `koanf:"imap"`        // port 143, STARTTLS
	IMAPS       *ServiceConfig `koanf:"imaps"`       // port 993, SSL
	Submission  *ServiceConfig `koanf:"submission"`  // port 587, STARTTLS outbound
	Submissions *ServiceConfig `koanf:"submissions"` // port 465, SSL outbound
	POP3        *ServiceConfig `koanf:"pop3"`        // port 110, STARTTLS
	POP3S       *ServiceConfig `koanf:"pop3s"`       // port 995, SSL
	LMTP        *ServiceConfig `koanf:"lmtp"`        // port 24, local delivery (no auth, loopback only)
}

// ProtocolConfig holds protocol-level behaviour settings, independent of listener.
type ProtocolConfig struct {
	IMAP       IMAPProtocolConfig       `koanf:"imap"`
	POP3       POP3ProtocolConfig       `koanf:"pop3"`
	Submission SubmissionProtocolConfig `koanf:"submission"`
	LMTP       LMTPProtocolConfig       `koanf:"lmtp"`
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

// AnvilServiceConfig configures the standalone yarilo-anvil process.
type AnvilServiceConfig struct {
	Listen   string         `koanf:"listen"`
	Shutdown ShutdownConfig `koanf:"shutdown"`
	// FailOpen controls login-pod behaviour when yarilo-anvil is unreachable.
	// true = allow the session; false (default) = reject the session.
	FailOpen bool `koanf:"fail_open"`
}

// AuthServiceConfig configures the standalone yarilo-auth process.
type AuthServiceConfig struct {
	Listen   string         `koanf:"listen"`
	Shutdown ShutdownConfig `koanf:"shutdown"`
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
}

// ShutdownConfig controls graceful shutdown behaviour.
type ShutdownConfig struct {
	SessionGracePeriod int `koanf:"session_grace_period"` // seconds to drain sessions before exit
	KillTimeout        int `koanf:"kill_timeout"`         // seconds after grace before SIGKILL
}

type AuthConfig struct {
	Passdb []PassdbEntry `koanf:"passdb"`
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
			XClient: XClientConfig{TrustedNets: defaultTrustedNets},
			Limits:  LimitsConfig{MaxUserIPConnections: 10},
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
		Telemetry: TelemetryConfig{Listen: ":8080"},
		Log:       LogConfig{Level: "info"},
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
