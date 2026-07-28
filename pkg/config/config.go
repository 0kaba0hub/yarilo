package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/0kaba0hub/yarilo/pkg/quota"
)

// Config is the top-level yarilo configuration.
type Config struct {
	Mode               string                       `koanf:"mode"` // legacy single-binary; ignored by multi-process binaries
	General            GeneralConfig                `koanf:"general"`
	Services           ServicesConfig               `koanf:"services"`
	Protocol           ProtocolConfig               `koanf:"protocol"`
	Auth               AuthConfig                   `koanf:"auth"`
	InternalTLS        InternalTLSConfig            `koanf:"internal_tls"`
	AuthService        AuthServiceConfig            `koanf:"auth_service"`
	AnvilService       AnvilServiceConfig           `koanf:"anvil_service"`
	DirectorService    DirectorServiceConfig        `koanf:"director_service"`
	BackendRegister    BackendRegisterConfig        `koanf:"backend_register"`
	IMAPLoginService   IMAPLoginServiceConfig       `koanf:"imap_login_service"`
	POP3LoginService   POP3LoginServiceConfig       `koanf:"pop3_login_service"`
	SubmissionLoginSvc SubmissionLoginServiceConfig `koanf:"submission_login_service"`
	LMTPLoginService   LMTPLoginServiceConfig       `koanf:"lmtp_login_service"`
	LocksService       LocksServiceConfig           `koanf:"locks_service"`
	FTS                FTSConfig                    `koanf:"fts"`
	LocksClient        LocksClientConfig            `koanf:"locks_client"`
	Storage            StorageConfig                `koanf:"storage"`
	Namespaces         []NamespaceConfig            `koanf:"namespaces"`
	// ACL is a mail-storage concern shared across protocols (IMAP RFC 4314
	// commands, and — as enforcement lands — LMTP post/insert, POP3 read),
	// so it lives at the top level rather than under protocol.imap.
	ACL                     ACLConfig                     `koanf:"acl"`
	Quota                   QuotaConfig                   `koanf:"quota"`
	Dicts                   map[string]DictConfig         `koanf:"dicts"`
	BackendAPI              BackendAPIConfig              `koanf:"backend_api"`
	QuotaStatus             QuotaStatusConfig             `koanf:"quota_status"`
	SASLLogin               SASLLoginConfig               `koanf:"sasl_login"`
	Login                   LoginConfig                   `koanf:"login"`
	Sieve                   SieveConfig                   `koanf:"sieve"`
	ManageSieveLoginService ManageSieveLoginServiceConfig `koanf:"managesieve_login_service"`
	Telemetry               TelemetryConfig               `koanf:"telemetry"`
	Log                     LogConfig                     `koanf:"log"`
}

// SieveConfig controls per-user Sieve email filtering (RFC 5228) in the LMTP delivery path.
type SieveConfig struct {
	// Enabled activates Sieve script execution during LMTP delivery.
	Enabled bool `koanf:"sieve_enabled"`
	// MaxScriptSize is the maximum compiled script size in bytes. Default: 65536.
	MaxScriptSize int `koanf:"sieve_max_script_size"`
	// MaxRedirects is the maximum number of redirect actions per message. Default: 32.
	MaxRedirects int `koanf:"sieve_max_redirects"`
	// MaxActions caps the total number of actions a single script may apply
	// (fileinto, redirect, keep, ...). Guards runaway scripts. 0 = unlimited.
	// Corresponds to sieve_max_actions. Default: 32.
	MaxActions int `koanf:"sieve_max_actions"`
	// DuplicateMaxPeriod caps the duplicate test's tracking period in seconds
	// (RFC 7352 §7: sites SHOULD impose a maximum; a larger :seconds is silently
	// clamped). 0 = no limit. Default: 604800 (7 days).
	DuplicateMaxPeriod int `koanf:"sieve_duplicate_max_period"`
	// DuplicateDriver selects the backend for the duplicate test (RFC 7352):
	//   file   — per-user file in the home dir (default; cross-pod on shared storage)
	//   memory — per-process, single-pod only
	//   redis  — the sieve_duplicate dict (cross-pod)
	DuplicateDriver string `koanf:"sieve_duplicate_driver"`
	// DuplicateFile is the name of the home-dir file for DuplicateDriver "file".
	// Default: ".yarilo.sieve-duplicate".
	DuplicateFile string `koanf:"sieve_duplicate_file"`
	// VacationEnabled permits the vacation extension (RFC 5230). Default: true.
	VacationEnabled bool `koanf:"sieve_vacation_enabled"`

	// SubmissionHost is the upstream MTA address (host[:port]) used to send
	// outbound mail for Sieve redirect and vacation actions. Default port 25.
	// Empty string disables outbound sending (redirect/vacation are silently dropped).
	SubmissionHost string `koanf:"sieve_submission_host"`
	// SubmissionSSL controls transport security: no | smtps | starttls. Default: no.
	SubmissionSSL string `koanf:"sieve_submission_ssl"`
	// SubmissionTimeout is the connect and command timeout in seconds. Default: 30.
	SubmissionTimeout int `koanf:"sieve_submission_timeout"`
	// SubmissionAuthSecret is the name of the Kubernetes Secret that holds
	// SMTP AUTH credentials (keys: user, password). Empty = no authentication.
	// The Helm chart mounts the secret as YARILO_SIEVE_SUBMISSION_USER /
	// YARILO_SIEVE_SUBMISSION_PASSWORD env vars based on this name.
	SubmissionAuthSecret string `koanf:"sieve_submission_auth_secret"`

	// DefaultName is the reserved name of the per-user default Sieve script
	// (the active-pointer entry point). Corresponds to sieve_default_name.
	// Default: "yarilo".
	DefaultName string `koanf:"sieve_default_name"`

	// GlobalBefore is an ordered list of paths to .sieve script files executed
	// before the user's active script. Admin-defined rules; applied to every
	// message regardless of per-user settings.
	GlobalBefore []string `koanf:"sieve_global_before"`
	// GlobalAfter is an ordered list of paths to .sieve script files executed
	// after the user's active script.
	GlobalAfter []string `koanf:"sieve_global_after"`

	// ImapSieveEnabled activates imapsieve (RFC 6785): running Sieve scripts on
	// IMAP events (message APPEND, COPY/MOVE, flag change). Per-mailbox binding
	// is via the IMAP METADATA annotation /shared/imapsieve/script (and the
	// server-wide equivalent under INBOX), not static config.
	ImapSieveEnabled bool `koanf:"imapsieve_enabled"`
	// ImapSieveScriptDir is the directory holding the admin-managed scripts a
	// mailbox's /shared/imapsieve/script annotation names (value "<name>" →
	// <dir>/<name>.sieve).
	ImapSieveScriptDir string `koanf:"imapsieve_script_dir"`
	// ImapSieveGlobalBefore / ImapSieveGlobalAfter are ordered .sieve paths run
	// before / after the mailbox-bound script on every imapsieve event.
	ImapSieveGlobalBefore []string `koanf:"imapsieve_global_before"`
	ImapSieveGlobalAfter  []string `koanf:"imapsieve_global_after"`

	// SieveExtensions is the whitelist of Sieve extensions users may declare
	// with require. Corresponds to sieve_extensions.
	// Empty slice = allow all extensions (backwards-compatible default).
	// Non-empty = strict whitelist enforced at PUTSCRIPT and delivery time.
	SieveExtensions []string `koanf:"sieve_extensions"`

	// ScriptsDriver selects the script storage backend: "fs" (default) stores
	// scripts as files in the user's home directory; "redis" uses the dict
	// instance named by ScriptsDictName.
	ScriptsDriver string `koanf:"sieve_scripts_driver"`
	// ScriptsDictName is the key in Config.Dicts that points to the dict
	// instance used when ScriptsDriver is "redis". Ignored for "fs".
	ScriptsDictName string `koanf:"sieve_scripts_dict"`

	// Environments is an operator-defined set of key-value pairs exposed to
	// Sieve scripts via the vnd.yarilo.environment extension as
	// vnd.yarilo.config.<key> items.
	Environments map[string]string `koanf:"sieve_environment"`

	// PipeBinDir is the directory where yarilo looks for executables to run
	// via the vnd.yarilo.pipe action.
	PipeBinDir string `koanf:"sieve_pipe_bin_dir"`

	// PipeSocketDir is the directory where yarilo looks for Unix sockets to
	// connect to via the vnd.yarilo.pipe action. Searched before PipeBinDir.
	PipeSocketDir string `koanf:"sieve_pipe_socket_dir"`

	// PipeExecTimeout is the maximum number of seconds a piped program may
	// run before being killed.
	PipeExecTimeout int `koanf:"sieve_pipe_exec_timeout"`

	// PipeInputEOL controls the line ending written to the program's stdin:
	// "crlf" (default, matches RFC 5322) or "lf".
	PipeInputEOL string `koanf:"sieve_pipe_input_eol"`

	// FilterBinDir is the directory where yarilo looks for executables to run
	// via the vnd.yarilo.filter action.
	FilterBinDir string `koanf:"sieve_filter_bin_dir"`

	// FilterSocketDir is the directory where yarilo looks for Unix sockets to
	// connect to via the vnd.yarilo.filter action. Searched before FilterBinDir.
	FilterSocketDir string `koanf:"sieve_filter_socket_dir"`

	// FilterExecTimeout is the maximum number of seconds a filter program may
	// run before being killed.
	FilterExecTimeout int `koanf:"sieve_filter_exec_timeout"`

	// FilterInputEOL controls the line ending written to the filter program's stdin:
	// "crlf" (default, matches RFC 5322) or "lf".
	FilterInputEOL string `koanf:"sieve_filter_input_eol"`

	// ExecuteBinDir is the directory where yarilo looks for executables to run
	// via the vnd.yarilo.execute action.
	ExecuteBinDir string `koanf:"sieve_execute_bin_dir"`

	// ExecuteSocketDir is the directory where yarilo looks for Unix sockets to
	// connect to via the vnd.yarilo.execute action. Searched before ExecuteBinDir.
	ExecuteSocketDir string `koanf:"sieve_execute_socket_dir"`

	// ExecuteExecTimeout is the maximum number of seconds an execute program may
	// run before being killed.
	ExecuteExecTimeout int `koanf:"sieve_execute_exec_timeout"`

	// ExecuteInputEOL controls the line ending written to the execute program's stdin:
	// "crlf" (default, matches RFC 5322) or "lf".
	ExecuteInputEOL string `koanf:"sieve_execute_input_eol"`

	// SpamStatusHeader names the message header carrying the spam score for the
	// spamtest / spamtestplus extensions (RFC 5235), e.g. "X-Spam-Score". Empty
	// leaves spamtest unbacked (the test reports "not scanned").
	// Corresponds to sieve_spamtest_status_header.
	SpamStatusHeader string `koanf:"sieve_spamtest_status_header"`
	// SpamMaxValue is the raw header value treated as the top of the scale; the
	// score is normalised to 1..10 (or 1..100 with :percent). Default: 10.
	SpamMaxValue float64 `koanf:"sieve_spamtest_max_value"`
	// VirusStatusHeader names the header carrying the virus verdict for the
	// virustest extension (RFC 5235). Empty leaves virustest unbacked.
	// Corresponds to sieve_virustest_status_header.
	VirusStatusHeader string `koanf:"sieve_virustest_status_header"`
	// VirusMaxValue is the raw header value treated as the top of the 1..5
	// virus scale. Default: 5.
	VirusMaxValue float64 `koanf:"sieve_virustest_max_value"`
	// ReportUserAgent is the User-Agent field written into ARF feedback reports
	// generated by the vnd.yarilo.report extension (RFC 5965). Default: "yarilo".
	ReportUserAgent string `koanf:"sieve_report_user_agent"`
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
	// Different namespaces MAY use different separators.
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
	// IgnoreACL bypasses ACL enforcement for this namespace: rights are
	// not checked and no folders are hidden by lookup right, even when
	// acl.enabled is true. Useful for a trusted namespace (e.g. an admin
	// or public root) that should be fully accessible.
	IgnoreACL bool `koanf:"acl_ignore"`
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

// XClientConfig gates NATIVE inbound client-IP forwarding on the login pods
// (#742): IMAP ID fields (x-originating-ip) and POP3/Submission XCLIENT. It is
// the login_trusted_networks analogue and is SEPARATE from general.haproxy,
// which keeps its own trusted_nets — an operator picks per listener between the
// PROXY protocol (haproxy_protocol), native forwarding (xclient_protocol), or
// neither. Per-listener enable is ServiceConfig.XClient (xclient_protocol);
// this block is the global trust list a forward's source must fall inside.
//
// Precedence when BOTH mechanisms are active on a listener: the PROXY header is
// consumed first, so by the time XCLIENT/ID is read the socket peer already
// reflects the PROXY-rewritten address. The trusted-net check runs against THAT
// peer (it is the real adjacent hop), and the XCLIENT/ID forward wins as the
// final client IP applied to auth, allow_nets, anvil, and the backend preamble.
type XClientConfig struct {
	TrustedNets []string `koanf:"trusted_nets"` // CIDRs whose forwarded client IP (XCLIENT/ID) is trusted
}

type LimitsConfig struct {
	MaxUserIPConnections int `koanf:"mail_max_userip_connections"` // 0 = unlimited
}

// ServiceConfig is per-listener configuration.
// A nil pointer in ServicesConfig means the listener is not started.
type ServiceConfig struct {
	Enabled         bool       `koanf:"enabled"`
	Port            int        `koanf:"port"`
	ConnectionLimit int        `koanf:"connection_limit"` // 0 = unlimited
	SSLMode         string     `koanf:"ssl_mode"`         // no | ssl | starttls
	SSL             *SSLConfig `koanf:"ssl"`              // overrides general.ssl
	HAProxy         bool       `koanf:"haproxy_protocol"`
	// XClient enables native inbound client-IP forwarding on this listener
	// (#742): IMAP ID x-originating-ip, POP3/Submission XCLIENT. A forward is
	// applied only when the socket peer is inside general.xclient.trusted_nets.
	// Off = the forwarding commands are ignored (ID replies NIL; XCLIENT is an
	// unknown command). See XClientConfig for the haproxy/native/none choice.
	XClient          bool `koanf:"xclient_protocol"`
	DisablePlainAuth bool `koanf:"disable_plaintext_auth"`
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
	// cluster-wide via yarilo-anvil at RCPT TO. Default: 10.
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
	// IMAPQuota toggles the IMAP QUOTA extension (RFC 9208): the QUOTA
	// capability advertisement + GETQUOTA / GETQUOTAROOT. Independent of the
	// quota engine (enforcement) — a client-facing protocol feature. Default on.
	IMAPQuota bool `koanf:"imap_quota"`
	// SpecialUseDefaults maps a folder name (case-sensitive) to its RFC 6154
	// special-use attribute. LIST advertises the attr automatically when the
	// folder name matches. Per-user CREATE (USE ...) overrides win against
	// these defaults via the on-disk special_use file.
	SpecialUseDefaults map[string]string `koanf:"imap_special_use_defaults"`
}

// ACLConfig groups RFC 4314 ACL knobs. Richer policy (global ACL,
// group= resolution, cache, etc.) lands in later ACL phases (see the ACL
// parity backlog).
type ACLConfig struct {
	Enabled bool `koanf:"enabled"`
	// DefaultsFromInbox makes root-level default ACLs resolve from INBOX's
	// ACL for private/shared namespaces. This is the maildir answer to the
	// namespace-root==INBOX collision, where the local folder-"" default is
	// unavailable.
	DefaultsFromInbox bool `koanf:"defaults_from_inbox"`
	// GlobalsOnly ignores the per-mailbox yarilo-acl files and evaluates only
	// the global ACL rules below. Useful for centrally-administered setups.
	GlobalsOnly bool `koanf:"globals_only"`
	// Global holds operator-configured ACL rules applied across all users
	// and merged with the per-mailbox ACL (global takes precedence).
	Global []GlobalACLRule `koanf:"global"`
	// CacheTTL is how long (seconds) a parsed per-mailbox ACL is trusted
	// before its file's mtime+size are re-validated (the acl_cache_ttl
	// knob, default 30). 0 disables caching — every right check reads the file.
	CacheTTL int `koanf:"acl_cache_ttl"`
}

// GlobalACLRule is one global ACL entry-set scoped to a mailbox name (or the
// "*" wildcard for every mailbox).
type GlobalACLRule struct {
	// Mailbox is the mailbox name this rule applies to, or "*" for all.
	Mailbox string `koanf:"mailbox"`
	// Entries are the (identifier, rights) grants; a leading "-" on rights
	// marks a negative-rights entry.
	Entries []GlobalACLEntry `koanf:"entries"`
}

// GlobalACLEntry is one identifier→rights grant within a GlobalACLRule.
type GlobalACLEntry struct {
	Identifier string `koanf:"identifier"`
	Rights     string `koanf:"rights"`
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
	MaxRecipients      int         `koanf:"max_recipients"` // 0 = unlimited
	RecipientDelimiter string      `koanf:"recipient_delimiter"`
	Workarounds        []string    `koanf:"client_workarounds"` // whitespace-before-path | mailbox-for-path | implicit-auth-external
	AddReceivedHeader  bool        `koanf:"submission_add_received_header"`
	Relay              RelayConfig `koanf:"relay"`
}

// RelayConfig holds SMTP relay settings (submission_relay_* knobs).
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
	// ServerName is the TLS name every internal CLIENT dial pins (#816).
	// Internal services are reached by short name OR FQDN OR pod IP depending
	// on the caller, so verifying against the dialed host is unreliable — the
	// shared internal cert instead carries one stable SAN, pinned here. Since
	// all internal components share one cert, mutual auth attests
	// "cluster member", not a specific service identity; a single pinned name
	// is the honest model (per-service identity would need per-service certs).
	// The director RING dial is the exception (director_service
	// .ring_tls_server_name, #753) — directors carry their own cert. Empty
	// with internal_tls enabled is a misconfiguration: mtls.ClientConfig fails
	// loudly at startup. The chart defaults it to <release>-internal.
	ServerName string `koanf:"server_name"`
	// SessionCacheSize is the TLS 1.3 client session-resumption cache size
	// (entries) for internal dials (#856). 0 uses the built-in default; a
	// negative value disables resumption (every dial pays a full handshake).
	SessionCacheSize int `koanf:"session_cache_size"`
	// SessionCacheTTL bounds how long (seconds) a cached session may be resumed,
	// on top of LRU eviction; 0 means LRU-only (no age limit). A TTL makes a
	// cert rotation stop resuming stale sessions within that window.
	SessionCacheTTL int `koanf:"session_cache_ttl"`
}

// QuotaConfig toggles the quota engine: enforcement on every save (IMAP
// APPEND/COPY/MOVE, LMTP delivery, and the quota-status policy service), summed
// from the index count backend. Independent of the IMAP QUOTA *extension*
// (protocol.imap.imap_quota), which only exposes the GETQUOTA commands.
type QuotaConfig struct {
	Enabled bool `koanf:"enabled"`
	// Name is the quota-root name surfaced in IMAP GETQUOTA / GETQUOTAROOT.
	// Empty falls back to "User quota".
	Name string `koanf:"quota_name"`
	// ExceededMessage is the text returned when a save is rejected for being
	// over quota (IMAP OVERQUOTA, LMTP 452, quota-status). Empty uses a default.
	ExceededMessage string `koanf:"quota_exceeded_message"`
	// MailSize rejects any single message larger than this (human size, e.g.
	// "50M"). Empty / "0" = unlimited. Independent of the usage limit.
	MailSize string `koanf:"quota_mail_size"`

	// StoragePercentage scales the resolved storage limit (limit*pct/100).
	// Default 100 (no scaling). Must be > 0.
	StoragePercentage int `koanf:"quota_storage_percentage"`
	// MessagePercentage scales the resolved message-count limit. Default 100.
	MessagePercentage int `koanf:"quota_message_percentage"`
	// StorageExtra is byte headroom added to the storage limit after the
	// percentage scaling (human size). Empty / "0" = none.
	StorageExtra string `koanf:"quota_storage_extra"`
	// Grace is the storage overshoot allowed past the limit on inbound delivery
	// (LMTP/LDA) only — never interactive IMAP (human size). Default "10M".
	Grace string `koanf:"quota_grace"`
	// IgnoreUnlimited omits the quota root from IMAP GETQUOTA/GETQUOTAROOT for a
	// user whose limits are all unlimited.
	IgnoreUnlimited bool `koanf:"quota_ignore_unlimited"`
	// MailboxCount caps the number of mailboxes (folders) a user may have.
	// 0 = unlimited. Enforced at folder creation.
	MailboxCount int64 `koanf:"quota_mailbox_count"`
	// MailboxMessageCount caps the number of messages in a single mailbox.
	// 0 = unlimited. Enforced on save.
	MailboxMessageCount int64 `koanf:"quota_mailbox_message_count"`
	// Hidden omits the quota root from IMAP GETQUOTA/GETQUOTAROOT for every user
	// (enforcement still applies).
	Hidden bool `koanf:"quota_hidden"`
	// WarningBinDir is the directory holding quota_warning execute programs.
	// Empty disables program execution (warnings then only log).
	WarningBinDir string `koanf:"quota_warning_bin_dir"`
	// WarningExecTimeout bounds a warning program's runtime in seconds. Default 10.
	WarningExecTimeout int `koanf:"quota_warning_exec_timeout"`
	// Warnings are the quota_warning rules.
	Warnings []QuotaWarning `koanf:"quota_warnings"`
	// CloneDicts names the dicts (from the top-level dicts: map) that mirror the
	// authoritative usage. Empty disables cloning. yarilo fans out to all of
	// them at once (e.g. SQL + Redis). The mirror is advisory, never the source
	// of truth.
	CloneDicts []string `koanf:"quota_clone_dicts"`
	// CloneFlushDelay debounces clone writes: at most one mirror write per this
	// many seconds per session, plus a final flush on session close. Default 10.
	CloneFlushDelay int `koanf:"quota_clone_flush_delay"`
	// OverStatusMask is the wildcard the userdb quota_over_flag is matched
	// against to decide the flagged over state. Empty disables the check.
	OverStatusMask string `koanf:"quota_over_status_mask"`
	// OverStatusLazyCheck defers the over-status check from login to the first
	// quota operation.
	OverStatusLazyCheck bool `koanf:"quota_over_status_lazy_check"`
	// OverStatusExecute is the program (+ args) run from quota_warning_bin_dir
	// when the actual over-quota state diverges from the userdb flag.
	OverStatusExecute string `koanf:"quota_over_status_execute"`
}

// QuotaWarning is one quota_warning rule (fires an action when usage crosses a
// percentage of the resource limit).
type QuotaWarning struct {
	Name       string `koanf:"quota_warning_name"`
	Resource   string `koanf:"quota_warning_resource"`   // storage | message
	Threshold  string `koanf:"quota_warning_threshold"`  // over | under
	Percentage int    `koanf:"quota_warning_percentage"` // % of the limit
	Execute    string `koanf:"quota_warning_execute"`    // program (+ args) in the bin dir
}

// QuotaPolicy builds the runtime quota.Policy from the config, parsing sizes
// and applying percentage defaults.
func (q QuotaConfig) QuotaPolicy() quota.Policy {
	return quota.Policy{
		StoragePercentage:   q.StoragePercentage,
		MessagePercentage:   q.MessagePercentage,
		StorageExtra:        quota.ParseSize(q.StorageExtra),
		StorageGrace:        quota.ParseSize(q.Grace),
		IgnoreUnlimited:     q.IgnoreUnlimited,
		MailboxCount:        q.MailboxCount,
		MailboxMessageCount: q.MailboxMessageCount,
		Hidden:              q.Hidden,
		Warnings:            q.quotaWarnings(),
		OverStatus: quota.OverStatusPolicy{
			Mask:      q.OverStatusMask,
			LazyCheck: q.OverStatusLazyCheck,
			Execute:   q.OverStatusExecute,
		},
	}
}

func (q QuotaConfig) quotaWarnings() []quota.Warning {
	if len(q.Warnings) == 0 {
		return nil
	}
	out := make([]quota.Warning, len(q.Warnings))
	for i, w := range q.Warnings {
		out[i] = quota.Warning{
			Name:       w.Name,
			Resource:   w.Resource,
			Threshold:  w.Threshold,
			Percentage: w.Percentage,
			Execute:    w.Execute,
		}
	}
	return out
}

// QuotaStatusConfig configures the yarilo-quota-status Postfix policy service.
type QuotaStatusConfig struct {
	// Listen is the TCP address the policy service binds to.
	// Postfix connects here via check_policy_service.
	// Default: ":12340"
	Listen string `koanf:"listen"`
	// RecipientDelimiter is the address detail separator used to derive the
	// target folder (alice+Spam@ → Spam). Default "+".
	RecipientDelimiter string `koanf:"recipient_delimiter"`
	// Nouser is the policy action returned when the recipient is unknown in
	// userdb. Default "REJECT Unknown user"; empty falls back to DUNNO.
	Nouser string `koanf:"quota_status_nouser"`
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
// yarilo-sasl-login listens for plain-TCP connections from Postfix (yarilo
// SASL auth protocol, smtpd_sasl_type=dovecot) and proxies each session to
// yarilo-auth, optionally wrapping the upstream connection with mTLS.
// This keeps the yarilo-auth socket internal — Postfix has no direct access.
// LoginConfig holds settings shared by every login proxy (imap/pop3/lmtp/
// submission/managesieve/sasl), independent of protocol.
type LoginConfig struct {
	// LookupHoldMax bounds how many times a login proxy re-LOOKUPs while the
	// director holds the user under a confirmed kick (#847/#858). The total hold
	// budget (LookupHoldMax × LookupHoldBackoff) MUST exceed the director's
	// worst-case confirm time (director_service.user_kill_confirm_grace + drain),
	// or the proxy exhausts its retries and errors the concurrent login before
	// the kill can confirm. 0 uses the default (20). With the default 150ms
	// backoff that is a 3s budget, covering the default 1s confirm grace + drain.
	LookupHoldMax int `koanf:"lookup_hold_max"`
	// LookupHoldBackoffMs is the delay (milliseconds) between LOOKUP hold
	// retries. 0 uses the default (150).
	LookupHoldBackoffMs int `koanf:"lookup_hold_backoff_ms"`
	// SessionGracePeriod is how long (seconds) a login proxy keeps serving
	// in-flight proxied sessions after SIGTERM before closing them, so a rolling
	// restart does not sever live sessions mid-command (#857). Must fit within
	// the pod terminationGracePeriodSeconds. 0 uses the default (30).
	SessionGracePeriod int `koanf:"session_grace_period"`
}

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

// FTSConfig configures full-text search: the engine selection, the
// yarilo-fts service topology and the indexing/search behaviour. The engine
// is required when enabled — startup fails fast on a missing or unknown name
// so the active engine is always stated in config. See docs/FTS.md.
type FTSConfig struct {
	Enabled bool `koanf:"enabled"`
	// Engine selects the active FTS engine: "flatcurve" (Xapian, cgo image)
	// or "bleve" (a follow-up stream). No implicit default.
	Engine string `koanf:"fts_engine"`

	// Mode / Addr / Listen follow the locks_service topology model:
	// remote = a yarilo-fts Deployment, embedded = in-process (tests/CLI).
	Mode   string `koanf:"fts_mode"`
	Addr   string `koanf:"fts_addr"`
	Listen string `koanf:"fts_listen"`
	// AuthMasterAddr is the yarilo-auth master listener for userdb lookups
	// (storage identity of the user being indexed). Empty = resolver defaults.
	AuthMasterAddr string `koanf:"fts_auth_master_addr"`

	Autoindex              bool     `koanf:"fts_autoindex"`
	AutoindexMaxRecentMsgs int      `koanf:"fts_autoindex_max_recent_msgs"`
	MessageMaxSize         int64    `koanf:"fts_message_max_size"`
	HeaderIncludes         []string `koanf:"fts_header_includes"`
	HeaderExcludes         []string `koanf:"fts_header_excludes"`
	CommitLimit            int      `koanf:"fts_commit_limit"`

	SearchAddMissing   string `koanf:"fts_search_add_missing"`
	SearchReadFallback bool   `koanf:"fts_search_read_fallback"`
	SearchTimeoutSecs  int    `koanf:"fts_search_timeout_secs"`
	SearchStrict       bool   `koanf:"fts_search_strict"`
	// Search disables FTS SEARCH while indexing keeps running (#726 item
	// 3) — incident degradation (bad query results, engine misbehaving)
	// without losing index freshness. Sessions treat Search=false as "no
	// FTS filter" (sequential scan); autoindex/write-through indexing is
	// unaffected, since it never checks this flag.
	Search bool `koanf:"fts_search"`

	Languages       []string `koanf:"languages"`
	LanguageFilters []string `koanf:"language_filters"`
	// LanguageFiltersOverride replaces LanguageFilters for specific
	// languages (#726 item 4) — e.g. a language with no Snowball stemmer
	// (uk) shouldn't carry "snowball" in its chain even though other
	// configured languages do. A language absent from this map uses
	// LanguageFilters unchanged; a present language's list is a full
	// replacement, not a merge. Every key must also appear in Languages —
	// validated at chain construction (catches typos like "ukr").
	LanguageFiltersOverride map[string][]string `koanf:"fts_language_filters_override"`
	// LanguageTokenMaxLen / LanguageAddressMaxLen (#726 item 1) are the
	// generic/address tokenizer byte caps, 0 = language package defaults
	// (30 / 250) — the most common operator tunings for index size vs.
	// long-token searchability.
	LanguageTokenMaxLen   int `koanf:"fts_language_tokenizer_generic_token_maxlen"`
	LanguageAddressMaxLen int `koanf:"fts_language_tokenizer_address_token_maxlen"`
	// LanguageTokenizerAlgorithm (#726 item 2): "simple" (default, the only
	// one implemented) or "tr29" — accepted but rejected at startup with a
	// clear error until the TR29 tokenizer lands (blocked on the Bleve
	// stream). LanguageTokenizerWB5A / LanguageTokenizerExplicitPrefix are
	// TR29-only knobs, also accepted-but-rejected-if-true for the same
	// reason: a silent no-op would be worse than a clear startup error.
	LanguageTokenizerAlgorithm      string `koanf:"fts_language_tokenizer_generic_algorithm"`
	LanguageTokenizerWB5A           bool   `koanf:"fts_language_tokenizer_generic_wb5a"`
	LanguageTokenizerExplicitPrefix bool   `koanf:"fts_language_tokenizer_generic_explicit_prefix"`

	FlatcurveCommitLimit int `koanf:"fts_flatcurve_commit_limit"`
	FlatcurveMinTermSize int `koanf:"fts_flatcurve_min_term_size"`
	// FlatcurveOptimizeLimit queues a mailbox for automatic background
	// shard compaction once its sealed-shard count reaches this value
	// (#715), in addition to the manual `yarctl fts optimize`
	// command. 0 explicitly disables auto-optimize (manual only) — this is
	// NOT defaulted at the flatcurve.Options layer, only here, so an
	// operator's explicit 0 is respected rather than silently coerced back
	// to the default.
	FlatcurveOptimizeLimit   int  `koanf:"fts_flatcurve_optimize_limit"`
	FlatcurveRotateCount     int  `koanf:"fts_flatcurve_rotate_count"`
	FlatcurveRotateTimeMsecs int  `koanf:"fts_flatcurve_rotate_time"`
	FlatcurveSubstringSearch bool `koanf:"fts_flatcurve_substring_search"`

	// DecoderDriver selects the external attachment-text-extraction backend:
	// "none" (default — attachments stay unindexed beyond HTML/text parts),
	// "script" (a yarilo-owned line protocol over DecoderScriptAddr), or
	// "tika" (HTTP to an Apache Tika server at DecoderTikaURL). See #669.
	DecoderDriver string `koanf:"fts_decoder_driver"`
	// DecoderScriptAddr accepts "unix:///path/to.sock" (standalone/embedded,
	// a co-located decoder process) or "host:port" (k8s/backend, the decoder
	// runs as its own Deployment/Service) — mirrors pkg/locks' embedded-vs-
	// remote Dialer split, since a bare socket path doesn't fit a topology
	// where the decoder isn't co-located with yarilo-fts.
	DecoderScriptAddr string `koanf:"fts_decoder_script_addr"`
	// DecoderTikaURL is the base URL of an Apache Tika server, e.g.
	// "http://tika.yarilo-sb.svc.cluster.local:9998".
	DecoderTikaURL string `koanf:"fts_decoder_tika_url"`
	// DecoderMaxSize caps the attachment bytes sent to the decoder per part
	// (0 = unlimited). Independent of MessageMaxSize, which caps indexed text
	// AFTER decoding.
	DecoderMaxSize int64 `koanf:"fts_decoder_max_size"`
	// DecoderTimeoutSecs bounds a single decode call.
	DecoderTimeoutSecs int `koanf:"fts_decoder_timeout_secs"`
	// DecoderMaxAttempts bounds the tika driver's retry count against
	// transient failures (network errors, 5xx) before degrading (#697).
	// 0/unset = 2 (one retry), matching the reference implementation's own
	// Tika plugin default. Not used by the script driver, which has no
	// retry — a script error is always a hard failure.
	DecoderMaxAttempts int `koanf:"fts_decoder_max_attempts"`

	// DedupBodyParts skips re-tokenizing a body part whose normalized text
	// content was already indexed for the SAME message (multipart/alternative
	// text+html twins, a quoted block repeated within one body). Opt-in:
	// default false, since the reference implementation has no equivalent and
	// some operators may not want the extra per-part hashing. Cross-message
	// dedup is out of scope — it cannot be done without breaking per-message
	// search correctness (a term's posting list must include every message
	// that actually contains it). See #669.
	DedupBodyParts bool `koanf:"fts_dedup_body_parts"`

	// DetectionSampleBytes bounds how many raw bytes of each body/attachment
	// part are read up front to derive its language-detection sample
	// (0 = buildmail's own default). Only matters with 2+ Languages
	// configured. See #696.
	DetectionSampleBytes int `koanf:"fts_detection_sample_bytes"`
	// DetectionMinRunes overrides the minimum sample length (in runes) below
	// which detection is considered unreliable and falls back to the first
	// configured language (0 = language package's own default). See #696.
	DetectionMinRunes int `koanf:"fts_detection_min_runes"`
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
	// Vhosts is the ring weight 1..100 for this static backend (#740/#797).
	// 0 = director default; set explicitly so it is a real least_sessions
	// candidate (0 there means drain).
	Vhosts int `koanf:"vhosts"`
}

// DirectorAPIConfig configures the HTTP admin API on yarilo-director.
type DirectorAPIConfig struct {
	Listen      string   `koanf:"listen"`       // default ":9103"
	Token       string   `koanf:"token"`        // Bearer token; supports ${ENV_VAR}
	AllowedNets []string `koanf:"allowed_nets"` // CIDRs allowed to call the API
}

// DirectorServiceConfig configures the standalone yarilo-director process.
// BackendRegisterConfig configures the co-located pod's director registration
// (#776/#788). It is consumed by the yarilo-backend-reg sidecar (which owns the
// single BACKEND-UP for the pod IP) and by the protocol containers' readiness
// touchers. Empty DirectorAddr disables registration (non-cluster / standalone).
type BackendRegisterConfig struct {
	// DirectorAddr is the director ClusterIP Service "host:port" to
	// register against — any replica; the registration gossips ring-wide.
	DirectorAddr string `koanf:"director_addr"`
	// RegisterInterval paces the sidecar heartbeat (seconds); 0 = 10.
	RegisterInterval int `koanf:"register_interval"`
	// Tag places this backend in a routing pool = NFS shard (matches director
	// tags); it is NOT a protocol dimension (#788).
	Tag string `koanf:"tag"`
	// Vhosts is the ring weight (0 = director default 100).
	Vhosts int `koanf:"vhosts"`

	// ReadinessDir is the shared (emptyDir) directory where each protocol
	// container touches its readiness file and the sidecar reads them (#788).
	// Empty disables the readiness signal (single-process / standalone runs).
	ReadinessDir string `koanf:"readiness_dir"`
	// ReadinessTouchInterval is how often (seconds) a protocol container
	// re-touches its readiness file WHILE ready; 0 = 5.
	ReadinessTouchInterval int `koanf:"readiness_touch_interval"`
	// ReadinessStaleAfter is how old (seconds) a readiness file may be before
	// the sidecar treats that protocol as not-ready and withholds the pod's
	// heartbeat; 0 = 15 (≈ 3× the touch interval). Widen on slow nodes to avoid
	// false silence flapping the whole pod.
	ReadinessStaleAfter int `koanf:"readiness_stale_after"`
	// ReadinessProtocols is the set of protocol readiness files the sidecar
	// requires fresh before heartbeating (e.g. imap, pop3, submission, lmtp,
	// managesieve). Empty = the sidecar heartbeats unconditionally (no gate).
	// Zero-valued ReadinessTouchInterval / ReadinessStaleAfter default to 5s /
	// 15s in readyfile.Touch / readyfile.AllFresh respectively.
	ReadinessProtocols []string `koanf:"readiness_protocols"`
}

type DirectorServiceConfig struct {
	Listen       string             `koanf:"listen"`
	Shutdown     ShutdownConfig     `koanf:"shutdown"`
	UserExpire   int                `koanf:"user_expire"`   // seconds before user→backend mapping expires; 0 = 900
	PingInterval int                `koanf:"ping_interval"` // seconds between PING probes; 0 = 30
	PingTimeout  int                `koanf:"ping_timeout"`  // seconds to wait for PONG before closing; 0 = 10
	WriteTimeout int                `koanf:"write_timeout"` // seconds to bound a single client push/reply write (#704); 0 = 10, negative = disabled
	MailServers  []MailServerConfig `koanf:"mail_servers"`  // static backend list, loaded at startup
	// Peers is the seed list for joining the self-organizing ring (#750) —
	// "host:port" addresses tried in order until one accepts a DIRECTOR-JOIN.
	// Once joined, membership is maintained automatically via DIRECTOR-ADD/
	// REMOVE propagation, not further seed polling. In k8s this is normally
	// the stable "-director" ClusterIP (kube-proxy guarantees it resolves to
	// *some* live member); a manual list is a valid seed override for
	// non-k8s deployments too.
	Peers []string          `koanf:"peers"`
	API   DirectorAPIConfig `koanf:"api"`
	// RingSecret authenticates incoming DIRECTOR-JOIN requests via
	// HMAC-SHA256 (#750). Supports ${ENV_VAR} — generate one Secret per
	// release the same way director_service.api.token is (see
	// helm/templates/secret-director-ring.yaml). Empty disables ring auth:
	// every JOIN is rejected and this node can only run as a singleton.
	RingSecret string `koanf:"ring_secret"`
	// RingTLSServerName is the TLS ServerName used when dialling ring peers
	// (JOIN + right-neighbor + seed polls) under internal_tls (#753). Ring
	// peers are dialled by ephemeral pod IP, so without a stable name Go would
	// verify the peer cert against the pod IP and fail (no pod-IP SAN). Set it
	// to a name present in every director's internal-tls cert — the chart
	// defaults it to the headless <release>-director-ring Service. Empty with
	// internal_tls enabled and peers configured is a misconfiguration (the ring
	// cannot verify pod-IP peers): the director logs an ERROR at startup.
	RingTLSServerName string `koanf:"ring_tls_server_name"`
	// JoinAllowedNets restricts which source CIDRs a DIRECTOR-JOIN is accepted
	// from (#773) — the exact pattern of api.allowed_nets: empty = allow all
	// (unchanged behaviour), otherwise the joiner's source IP must fall inside
	// one of the listed networks or the JOIN is rejected before the HMAC
	// challenge even begins. A cheap first-line filter that keeps the ring-join
	// surface off untrusted networks; the dial-back check and HMAC proof are the
	// per-peer identity controls layered behind it.
	JoinAllowedNets []string `koanf:"join_allowed_nets"`
	// MinMembers is an install-time warning threshold only ("fewer members
	// than this = no state redundancy") — it never refuses service at any
	// member count. Default 3 (matches the reference's recommended minimum
	// for the degradation ladder to have real redundancy at rest).
	MinMembers int `koanf:"min_members"`
	// AntiEntropyInterval is how often (seconds) each ring member
	// re-broadcasts its member+tombstone snapshot over every live ring
	// connection (#759) — a bounded safety net that heals membership
	// splits without waiting for a possibly-lost ADD/REMOVE broadcast.
	// 0 = default (3); negative = disabled.
	AntiEntropyInterval int `koanf:"anti_entropy_interval"`
	// SeedPollInterval is how often (seconds) each member re-polls a seed
	// after its initial join (#759) — the seed is the one guaranteed
	// crossing point between partitioned member views, so this bounds any
	// formation split's lifetime regardless of ring dial topology. Runs
	// at full cadence while the view holds fewer than min_members,
	// easing to seed_poll_idle_interval once the expected cluster size is
	// reached (and snapping back on any loss); gating on the configured
	// target size — never on own-view stability, which a partitioned
	// node also exhibits. A hostname seed is resolved explicitly and
	// every resulting address except self is polled each cycle.
	// 0 = default (2); negative = legacy one-shot join.
	SeedPollInterval int `koanf:"seed_poll_interval"`
	// BackendExpire is how long (seconds) a lease-managed backend may go
	// without a heartbeat before it is removed ring-wide (#776). A backend
	// becomes lease-managed when a seq'd BACKEND-UP arrives for it (a
	// self-registering pod); static mail_servers and admin-added backends
	// never heartbeat and are never expired. 0 = default (30); negative =
	// disabled.
	BackendExpire int `koanf:"backend_expire"`
	// BackendUnreachableReporters is how many DISTINCT login proxies must
	// report a backend unreachable (dial failed) within
	// BackendUnreachableWindow before the director evicts it from the ring
	// ahead of the lease TTL (#782 — active fast-fail). Reports replicate
	// ring-wide, so the count aggregates across all directors. >1 guards
	// against a single partitioned proxy wrongly evicting a healthy backend;
	// the last backend of a tag is never evicted. 0 = default (2). Single
	// login-replica deployments (e.g. sandbox) should set this to 1, since two
	// distinct reporters for one protocol's failure may never exist — TTL
	// expiry (backend_expire) remains the backstop either way.
	BackendUnreachableReporters int `koanf:"backend_unreachable_reporters"`
	// BackendUnreachableWindow is the sliding window (seconds) over which those
	// distinct reports must arrive to corroborate. 0 = default (5).
	BackendUnreachableWindow int `koanf:"backend_unreachable_window"`
	// SeedPollIdleInterval is the eased poll cadence (seconds) once the
	// view has reached min_members. Defaults to the same 2s as
	// SeedPollInterval — no effective backoff — because a node cannot
	// tell "converged" from "stable but holding a dead member": a
	// freshly-respawned replacement pod that learned a since-dead member
	// during the death-detection window would otherwise keep it for a
	// full idle interval (#765). Raise it only to trade steady-state
	// polling for slower dead-member eviction on fresh joiners; clamped
	// up to seed_poll_interval. 0 = default (2).
	SeedPollIdleInterval int `koanf:"seed_poll_idle_interval"`
	// TombstoneTTL bounds (seconds) how long a dead member's tombstone is
	// kept and gossiped (#765) — churn across many rollouts must not grow
	// the set forever. Safe to expire: neighbor liveness monitoring (#768)
	// re-evicts a resurrected-but-unreachable member within seconds
	// regardless. 0 = default (600); negative = never expire.
	TombstoneTTL int `koanf:"tombstone_ttl"`
	// UsernameHashLowercase lowercases usernames before hashing/keying them
	// for ring routing, sticky assignments and admin overrides (#738).
	// Matches the reference implementation's default hash template — two
	// spellings of the same account ("User@d.test" / "user@d.test")
	// otherwise land on different backends. Default: true. Migration note:
	// enabling this on an already-running cluster changes hashes for any
	// mixed-case usernames; their existing sticky entries just expire
	// naturally via TTL (director_service.user_expire) — no special
	// migration step is needed.
	UsernameHashLowercase bool `koanf:"username_hash_lowercase"`
	// UsernameHash is the username→hash-key template (#850), mirroring the reference
	// director_username_hash expression so a dovecot.conf value migrates verbatim:
	// %u (whole user), %n (local part, before first '@'), %d (domain, after first '@'),
	// each with an optional %L lowercase modifier, plus %% for a literal percent.
	// Examples: "%Lu" (default, whole username lowercased), "%u" (case-sensitive),
	// "%Ld" (route the whole domain to one backend — shared-mailbox/ACL locality),
	// "%Ln" (local part only, for alias-domain installs). Empty derives the template
	// from username_hash_lowercase (%Lu / %u) for byte-identical back-compat. When set,
	// it — not the bool — governs case-folding. Invalid templates fail loudly at startup.
	UsernameHash     string `koanf:"username_hash"`
	AssignmentPolicy string `koanf:"assignment_policy"` // hash | least_sessions (#797); default hash
	// UserKickDelay is how long (seconds) an admin-initiated kick is delayed
	// before the USER-KICKED is pushed (#740), giving a user's in-flight
	// command on the old backend a grace window to complete after a move.
	// Applies ONLY to admin-initiated kicks (director API); a backend-down /
	// expiry kick fires immediately (there is nothing left to grace on a dead
	// backend) and the split-writer conflict-kick is likewise never delayed.
	// Matches the reference's director_user_kick_delay. 0 = default (2);
	// negative = disabled (immediate). There is deliberately no
	// max_parallel_moves equivalent: yarilo rehashes lazily (kick → re-login →
	// LOOKUP), so the move rate is already bounded by max_parallel_kicks —
	// a parsed-but-unread key would be a config gap, so it is omitted.
	UserKickDelay int `koanf:"user_kick_delay"`
	// UserKillTimeout is the hard fallthrough (seconds) for the confirmed
	// ring-wide kick (#847): while a user is being killed, LOOKUP is held so a
	// concurrent login cannot land on a fresh backend before the old sessions
	// are gone (the split-writer window). If the kill is not confirmed complete
	// within this window (a stuck session-holder), the killing flag is cleared
	// anyway — falling through to normal assignment with a WARN, so a user is
	// never permanently locked out. Replicated as a DURATION (each director
	// computes its own local deadline on receipt — never a wall-clock deadline,
	// which pod-clock skew would make unstable). 0 = default (15).
	UserKillTimeout int `koanf:"user_kill_timeout"`
	// UserKillConfirmGrace is how long (seconds) the user's ring-wide session
	// count must stay at zero before the kill is confirmed complete (#847). This
	// stable-zero window absorbs the race where a session routed just before the
	// kill does its SESSION-OPEN mid-window, momentarily dipping the count to
	// zero before that open lands — clearing on the first zero would let a new
	// login slip in. 0 = default (1).
	UserKillConfirmGrace int `koanf:"user_kill_confirm_grace"`
	// MaxParallelKicks caps how many sessions are kicked per batch when a
	// backend goes down (#740). The remaining sessions are kicked in
	// subsequent batches with a short pause between them, spreading the
	// re-login stampede across the surviving backends instead of firing every
	// kick at once. Matches the reference's director_max_parallel_kicks.
	// 0 = default (100); negative or 0-after-default disables batching (kick
	// all at once).
	MaxParallelKicks int `koanf:"max_parallel_kicks"`
	// MaxParallelMoves caps how many users are migrated concurrently during a
	// GRACEFUL backend evacuation (#849) — the throttled `flush` (no --force).
	// The director keeps at most this many user moves in flight at once; each
	// move completes (its old sessions confirm gone) before the next user is
	// pulled in, so a planned backend drain spreads the re-login across the
	// surviving pods instead of stampeding them all at once. Matches the
	// reference's director_max_parallel_moves. 0 = default (5); negative =
	// unlimited (all users moved at once, ~equivalent to --force but via moves).
	MaxParallelMoves int `koanf:"max_parallel_moves"`
	// FlushProgram is an optional external executable run once per user AFTER a
	// deliberate relocation (an admin USER-MOVE or a graceful evacuation) has been
	// confirmed ring-wide — i.e. after the user's old sessions are gone (#848).
	// Operators hook mailbox-cache flush, external session cleanup, metrics, etc.
	// It is invoked as: flush_program FLUSH <username> <username_hash> <old_backend>
	// <new_backend>, best-effort and asynchronous with a bounded timeout — a slow or
	// failing hook is logged, never blocks the ring/LOOKUP, and never fails the move.
	// Only the director that originated the move runs it (mirrors the reference's
	// self-initiated semantics); mass/reactive paths (backend-down, --force flush) do
	// NOT trigger it. Empty = disabled (default). The reference's director_flush_socket.
	FlushProgram string `koanf:"flush_program"`
	// LMTPListen enables the director's embedded LMTP proxy (per-recipient
	// fan-out via ring routing) on this address, e.g. ":10024". Empty =
	// disabled. This deliberately does NOT reuse the shared services.lmtp
	// block: that block belongs to the lmtp/lmtp-login pods, and gating the
	// director proxy on it forced the Helm chart to rewrite services.lmtp
	// whenever the director was enabled — silently breaking lmtp-login
	// (#748 item 1).
	LMTPListen string `koanf:"lmtp_listen"`
	// LMTPBackendPort is the LMTP port dialed on ring backends by the
	// embedded proxy. 0 = the port parsed from LMTPListen (the pre-#748
	// behavior, where both were services.lmtp.port).
	LMTPBackendPort int `koanf:"lmtp_backend_port"`
}

// IMAPLoginServiceConfig configures the yarilo-imap-login proxy.
// BackendAddr, when set, bypasses director LOOKUP and routes every
// session directly to this address (standalone k8s deployments).
// Leave empty in director deployments — set DirectorAddr instead.
//
// Precedence (#735): BackendAddr wins when both are set — an explicit
// standalone override always takes priority over director mode. This is
// login.Options' existing behavior (internal/login/server.go), kept
// unchanged; matches the direction #741 settles on for lmtp-login too.
type IMAPLoginServiceConfig struct {
	BackendAddr string `koanf:"backend_addr"`
	// DirectorAddr enables director mode: per-session LOOKUP via
	// yarilo-director (e.g. "yarilo-director:9102"). Ignored when
	// BackendAddr is set. At least one of BackendAddr/DirectorAddr must be
	// set (#735 — an empty DirectorAddr previously silently fell back to
	// this process's own DirectorService.Listen, dialing localhost where
	// no director runs).
	DirectorAddr string `koanf:"director_addr"`
	// BackendPort overrides the port returned by a director LOOKUP (the
	// backend's protocol-specific containerPort may differ from what the
	// director's ring tracks). 0 = use the LOOKUP result port as-is.
	BackendPort int `koanf:"backend_port"`
	// DirectorTag restricts LOOKUP to backends carrying this tag (#737).
	// Must match the tag of the backend pool this login Deployment serves
	// (DEPLOYMENT.md: one login pod = one tag-pool). "" = the untagged pool.
	DirectorTag string `koanf:"director_tag"`
}

// POP3LoginServiceConfig mirrors IMAPLoginServiceConfig for the POP3 proxy.
type POP3LoginServiceConfig struct {
	BackendAddr  string `koanf:"backend_addr"`
	DirectorAddr string `koanf:"director_addr"`
	BackendPort  int    `koanf:"backend_port"`
	DirectorTag  string `koanf:"director_tag"`
}

// SubmissionLoginServiceConfig mirrors IMAPLoginServiceConfig for the Submission proxy.
type SubmissionLoginServiceConfig struct {
	BackendAddr  string `koanf:"backend_addr"`
	DirectorAddr string `koanf:"director_addr"`
	BackendPort  int    `koanf:"backend_port"`
	DirectorTag  string `koanf:"director_tag"`
}

// ManageSieveLoginServiceConfig configures the yarilo-managesieve-login proxy (RFC 5804).
type ManageSieveLoginServiceConfig struct {
	// BackendAddr is the fixed address of the yarilo-managesieve backend.
	BackendAddr string `koanf:"backend_addr"`
	// DirectorAddr / BackendPort — see IMAPLoginServiceConfig.
	DirectorAddr string `koanf:"director_addr"`
	BackendPort  int    `koanf:"backend_port"`
	DirectorTag  string `koanf:"director_tag"`
	// HAProxy enables PROXY protocol v1/v2 header parsing.
	HAProxy bool `koanf:"haproxy_protocol"`
	// HAProxyTimeout is the read deadline for the PROXY header in seconds.
	HAProxyTimeout int `koanf:"haproxy_timeout"`
	// HAProxyNets lists CIDRs whose PROXY header is trusted.
	HAProxyNets []string `koanf:"haproxy_trusted_nets"`
}

// ValidateBackendOrDirector requires at least one of a login/proxy
// component's BackendAddr (standalone) or DirectorAddr (director mode) to
// be set (#735) — an empty DirectorAddr previously let these components
// silently fall back to this process's own in-process director bind
// address (DirectorService.Listen), dialing localhost where no director
// runs, rather than failing loudly at startup. component names the config
// section in the error message (e.g. "imap_login_service").
func ValidateBackendOrDirector(component, backendAddr, directorAddr string) error {
	if backendAddr == "" && directorAddr == "" {
		return fmt.Errorf("%s: set either backend_addr (standalone) or director_addr (director mode)", component)
	}
	return nil
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
	// DirectorTag restricts LOOKUP to backends carrying this tag (#737).
	// "" = the untagged pool, not "any tag" — there is no full-ring mode.
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
// is hosted by yarilo-director on its own port; the yarctl
// CLI surfaces both through nested subcommands (`yarctl
// director ...` vs `yarctl backend ...`).
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
	// the first failure. Default 3.
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
	Driver            string `koanf:"driver"`              // sqlite | mysql | postgres | passwd-file
	DSN               string `koanf:"dsn"`                 // SQL drivers
	PasswdFile        string `koanf:"passwd_file"`         // passwd-file driver: path to the file
	PasswordQuery     string `koanf:"password_query"`      // custom SELECT; %u/%n/%d substituted as parameters
	UserQuery         string `koanf:"user_query"`          // optional userdb lookup; %u/%n/%d substituted
	IterateQuery      string `koanf:"iterate_query"`       // optional list-users query (admin tooling)
	DefaultPassScheme string `koanf:"default_pass_scheme"` // assumed scheme when stored password has no {SCHEME} prefix (default PLAIN)
	SkipSchema        bool   `koanf:"skip_schema"`         // do not run CREATE TABLE IF NOT EXISTS on startup

	// static driver: one shared credential + templated fields for every user.
	StaticPassword string            `koanf:"static_password"` // shared password ({SCHEME} or default scheme)
	Nopassword     bool              `koanf:"nopassword"`      // accept any password (proxy front); requires empty static_password
	Fields         map[string]string `koanf:"fields"`          // templated fields (%u/%n/%d); userdb_-prefixed → userdb, bare → passdb
}

type StorageConfig struct {
	Mailbox          string `koanf:"mailbox"`
	MaildirRoot      string `koanf:"maildir_root"`
	MailHomeTemplate string `koanf:"mail_home_template"`
	MailPath         string `koanf:"mail_path"`
	MailInboxPath    string `koanf:"mail_inbox_path"`
	Index            string `koanf:"index"`
	IndexDir         string `koanf:"index_dir"`

	// MaildirSyncOnSelect reconciles the maildir index against the on-disk
	// cur/ and new/ directories on every SELECT/EXAMINE so messages delivered
	// or renamed out of band (MDA, another MUA) become visible without an
	// operator rebuild. Default true. Only the maildir driver honours it;
	// index-authoritative drivers (dbox) ignore it and self-heal reactively.
	MaildirSyncOnSelect bool `koanf:"maildir_sync_on_select"`

	// DboxReactiveRebuild enables reactive self-heal for sdbox: when a read hits
	// a missing/corrupt message the folder index is flagged and the next open
	// expunges the vanished records under the mailbox lock. Default true. Only
	// dbox honours it; maildir reconciles proactively via maildir_sync_on_select.
	// (mdbox reactive rebuild is phase 2 — see #594.)
	DboxReactiveRebuild bool `koanf:"dbox_reactive_rebuild"`

	// MaxConcurrentWrites caps the number of concurrent box.Save() calls
	// (message body writes to disk). Tune to match storage throughput:
	// spinning disks typically benefit from 16-32, SSDs from 128-256.
	// 0 means unlimited (default for backwards compatibility).
	MaxConcurrentWrites int `koanf:"max_concurrent_writes"`
	// MdboxAltStoragePath is the base directory for the mdbox alt
	// (cold) storage tier. Supports the same %u/%n/%d/%Lu/%Ln/%Ld
	// template variables as mail_home_template. Empty disables alt
	// storage (default).
	// Example: /mnt/cold/%d/%n
	MdboxAltStoragePath string `koanf:"mdbox_alt_storage_path"`

	// MdboxRotateSize is the maximum size of a single m.<N> file before a new save
	// rolls to a fresh file. Accepts a human-readable size ("10M", "1G") or a raw
	// byte count, parsed via quota.ParseSize at wiring time. Empty or "0" uses the
	// default (10 MiB).
	MdboxRotateSize string `koanf:"mdbox_rotate_size"`
	// MdboxRotateInterval rolls the current m.<N> file once it is older than this,
	// independent of size. Accepts a duration ("30s", "5m", "1h") or a raw second
	// count. Empty or "0" disables age-based rotation (default). The cutoff is a
	// rolling window (a file lives at least this long), not a clock-boundary snap.
	MdboxRotateInterval string `koanf:"mdbox_rotate_interval"`
	// MdboxPreallocateSpace reserves each new m.<N>'s space up front via
	// fallocate() (Linux only) instead of growing it write-by-write. Default false.
	MdboxPreallocateSpace bool `koanf:"mdbox_preallocate_space"`

	// VolatileDir is the cluster-wide VOLATILEDIR template. When set,
	// the fileindex Recreate tmp file is written here (typically a
	// local tmpfs) and then copied to NFS, keeping the expensive fsync
	// off the NFS path. Supports %u/%n/%d/%h template variables.
	// Example: /run/yarilo-volatile/%d/%n
	VolatileDir string `koanf:"volatile_dir"`

	// IndexLogCompactMinBytes is the minimum .index.log size at which
	// automatic compaction (flush base + truncate log) may occur.
	// Compaction only fires when the log is also older than
	// IndexLogCompactMinAgeSecs (age guard prevents burst storms).
	// 0 disables compaction entirely. Default 32 KiB.
	IndexLogCompactMinBytes int64 `koanf:"index_log_compact_min_bytes"`
	// IndexLogCompactMaxBytes forces compaction regardless of log age
	// when the log exceeds this size. Default 1 MiB.
	IndexLogCompactMaxBytes int64 `koanf:"index_log_compact_max_bytes"`
	// IndexLogCompactMinAgeSecs is the minimum log age in seconds
	// before a min-size compaction fires. Default 300 s.
	IndexLogCompactMinAgeSecs int `koanf:"index_log_compact_min_age_secs"`

	// ControlDir is the cluster-wide CONTROL= template. When set,
	// per-folder control files (yarilo-uidlist, subscriptions) are
	// stored here instead of co-located with the mailbox data under
	// home. Supports %u/%n/%d/%h template variables.
	// Example: /var/yarilo-control/%d/%n
	ControlDir string `koanf:"control_dir"`

	// AltDir is the cluster-wide ALT= template. When set, enables
	// two-tier maildir storage: messages cold-tiered via altmove
	// live here; reads check both primary (home) and alt tiers.
	// Supports %u/%n/%d/%h template variables.
	// Example: /mnt/cold/%d/%n
	AltDir string `koanf:"alt_dir"`

	// MailboxListUTF8 controls on-disk folder name encoding.
	// true (default): folder names are stored as UTF-8 on the filesystem.
	// false: folder names use modified-UTF-7 (RFC 3501 §5.1.3), needed
	// only when migrating from a legacy installation that already stores
	// them in that encoding.
	MailboxListUTF8 bool `koanf:"mailbox_list_utf8"`

	// MailboxListNormalizeToNFC applies Unicode NFC normalization to folder
	// names before they are stored or compared. Enabled by default so that
	// equivalent-but-differently-composed names (e.g. from macOS HFS+) are
	// treated as the same folder.
	MailboxListNormalizeToNFC bool `koanf:"mailbox_list_normalize_to_nfc"`
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
				IMAPQuota:          true,
				// Conventional special-use mappings.
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
		Storage: StorageConfig{
			MaildirSyncOnSelect: true,
			DboxReactiveRebuild: true,
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
			UserExpire:                  900,
			PingInterval:                30,
			PingTimeout:                 10,
			WriteTimeout:                10,
			UsernameHashLowercase:       true,
			AssignmentPolicy:            "hash",
			UserKickDelay:               2,
			UserKillTimeout:             15,
			UserKillConfirmGrace:        1,
			MaxParallelKicks:            100,
			MaxParallelMoves:            5,
			MinMembers:                  3,
			AntiEntropyInterval:         3,
			SeedPollInterval:            2,
			SeedPollIdleInterval:        2,
			BackendExpire:               30,
			BackendUnreachableReporters: 2,
			BackendUnreachableWindow:    5,
			TombstoneTTL:                600,
			API: DirectorAPIConfig{
				Listen: ":9103",
				// No default IP restriction — service/pod CIDRs differ per
				// cluster/CNI, so a hardcoded guess here (kubeadm's
				// 10.96.0.0/12 + 10.244.0.0/16 was tried and silently
				// 403'd every request on clusters using different ranges,
				// #759) is either wrong out of the box or requires knowing
				// the cluster's CIDRs before the config even works. The
				// auto-generated bearer token is the real security
				// boundary; allowed_nets is opt-in defense-in-depth once
				// an operator knows their actual CIDRs.
				AllowedNets: nil,
			},
		},
		QuotaStatus: QuotaStatusConfig{Listen: ":12340", RecipientDelimiter: "+", Nouser: "REJECT Unknown user"},
		Quota: QuotaConfig{
			Name:              "User quota",
			ExceededMessage:   "Quota exceeded (mailbox for user is full)",
			StoragePercentage: 100,
			MessagePercentage: 100,
			Grace:             "10M",
			CloneFlushDelay:   10,
		},
		FTS: FTSConfig{
			Mode:                       "remote",
			Listen:                     ":9106",
			CommitLimit:                500,
			SearchAddMissing:           "body-search-only",
			SearchReadFallback:         true,
			SearchTimeoutSecs:          30,
			Search:                     true,
			Languages:                  []string{"en"},
			LanguageFilters:            []string{"lowercase", "snowball", "stopwords"},
			LanguageTokenizerAlgorithm: "simple",

			FlatcurveCommitLimit:     500,
			FlatcurveMinTermSize:     2,
			FlatcurveOptimizeLimit:   10,
			FlatcurveRotateCount:     5000,
			FlatcurveRotateTimeMsecs: 5000,

			DecoderDriver:      "none",
			DecoderTimeoutSecs: 30,
		},
		SASLLogin: SASLLoginConfig{
			Listen:         ":12325",
			HAProxyTimeout: 3,
		},
		Telemetry: TelemetryConfig{Listen: ":8080"},
		Log:       LogConfig{Level: "info"},
		ACL:       ACLConfig{CacheTTL: 30},
		Sieve: SieveConfig{
			DefaultName:        "yarilo",
			MaxScriptSize:      65536,
			MaxRedirects:       32,
			MaxActions:         32,
			DuplicateMaxPeriod: 604800, // 7 days
			DuplicateDriver:    "file",
			DuplicateFile:      ".yarilo.sieve-duplicate",
			VacationEnabled:    true,
			SpamMaxValue:       10,
			VirusMaxValue:      5,
			ReportUserAgent:    "yarilo",
			SubmissionSSL:      "no",
			SubmissionTimeout:  30,
			PipeBinDir:         "/usr/lib/yarilo/sieve-pipe",
			PipeSocketDir:      "sieve-pipe",
			PipeExecTimeout:    10,
			PipeInputEOL:       "crlf",
			FilterBinDir:       "/usr/lib/yarilo/sieve-filter",
			FilterSocketDir:    "sieve-filter",
			FilterExecTimeout:  10,
			FilterInputEOL:     "crlf",
			ExecuteBinDir:      "/usr/lib/yarilo/sieve-execute",
			ExecuteSocketDir:   "sieve-execute",
			ExecuteExecTimeout: 10,
			ExecuteInputEOL:    "crlf",
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
	// A 0 value silently disables the per-user concurrency guard,
	// which is a multi-tenant footgun. Force operators to pick:
	// leave default (10), set a positive integer, or -1 for
	// "unlimited".
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
	cfg.DirectorService.RingSecret = expand(cfg.DirectorService.RingSecret)
	cfg.BackendAPI.Token = expand(cfg.BackendAPI.Token)
	cfg.Protocol.Submission.Relay.Password = expand(cfg.Protocol.Submission.Relay.Password)
	for i := range cfg.Auth.Passdb {
		cfg.Auth.Passdb[i].DSN = expand(cfg.Auth.Passdb[i].DSN)
		cfg.Auth.Passdb[i].PasswdFile = expand(cfg.Auth.Passdb[i].PasswdFile)
		cfg.Auth.Passdb[i].StaticPassword = expand(cfg.Auth.Passdb[i].StaticPassword)
	}
	for i := range cfg.Auth.MasterUsers.Masterdb {
		cfg.Auth.MasterUsers.Masterdb[i].DSN = expand(cfg.Auth.MasterUsers.Masterdb[i].DSN)
	}
	// Dict connection settings (dsn, addr, password, ...) commonly come from a
	// secret via ${ENV}. Settings is a shared map, so mutating it in place is
	// visible through cfg.Dicts.
	for _, dc := range cfg.Dicts {
		for k, v := range dc.Settings {
			if s, ok := v.(string); ok {
				dc.Settings[k] = expand(s)
			}
		}
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
