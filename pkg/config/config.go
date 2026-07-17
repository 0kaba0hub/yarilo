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

	Languages       []string `koanf:"languages"`
	LanguageFilters []string `koanf:"language_filters"`

	FlatcurveCommitLimit     int  `koanf:"fts_flatcurve_commit_limit"`
	FlatcurveMinTermSize     int  `koanf:"fts_flatcurve_min_term_size"`
	FlatcurveOptimizeLimit   int  `koanf:"fts_flatcurve_optimize_limit"`
	FlatcurveRotateCount     int  `koanf:"fts_flatcurve_rotate_count"`
	FlatcurveRotateTimeMsecs int  `koanf:"fts_flatcurve_rotate_time"`
	FlatcurveSubstringSearch bool `koanf:"fts_flatcurve_substring_search"`
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

	// DboxReactiveRebuild enables reactive auto-rebuild for the dbox drivers:
	// when a read hits a missing/corrupt message the folder index is flagged and
	// the next open rebuilds it from storage. Default true. Only dbox honours it;
	// maildir reconciles proactively via maildir_sync_on_select instead.
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
			Mode:               "remote",
			Listen:             ":9106",
			CommitLimit:        500,
			SearchAddMissing:   "body-search-only",
			SearchReadFallback: true,
			SearchTimeoutSecs:  30,
			Languages:          []string{"en"},
			LanguageFilters:    []string{"lowercase", "snowball", "stopwords"},

			FlatcurveCommitLimit:     500,
			FlatcurveMinTermSize:     2,
			FlatcurveOptimizeLimit:   10,
			FlatcurveRotateCount:     5000,
			FlatcurveRotateTimeMsecs: 5000,
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
