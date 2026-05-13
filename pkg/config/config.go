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
	Mode      string          `koanf:"mode"` // proxy | director | backend | single
	General   GeneralConfig   `koanf:"general"`
	Services  ServicesConfig  `koanf:"services"`
	Protocol  ProtocolConfig  `koanf:"protocol"`
	Auth      AuthConfig      `koanf:"auth"`
	Storage   StorageConfig   `koanf:"storage"`
	Telemetry TelemetryConfig `koanf:"telemetry"`
	Log       LogConfig       `koanf:"log"`
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
	SMTP        *ServiceConfig `koanf:"smtp"`        // port 25, MX inbound
	Submission  *ServiceConfig `koanf:"submission"`  // port 587, STARTTLS outbound
	Submissions *ServiceConfig `koanf:"submissions"` // port 465, SSL outbound
	POP3        *ServiceConfig `koanf:"pop3"`        // port 110, STARTTLS
	POP3S       *ServiceConfig `koanf:"pop3s"`       // port 995, SSL
	LMTP        *ServiceConfig `koanf:"lmtp"`        // port 24, local delivery (no auth, loopback only)
}

// ProtocolConfig holds protocol-level behaviour settings, independent of listener.
type ProtocolConfig struct {
	IMAP IMAPProtocolConfig `koanf:"imap"`
	POP3 POP3ProtocolConfig `koanf:"pop3"`
	SMTP SMTPProtocolConfig `koanf:"smtp"`
	LMTP LMTPProtocolConfig `koanf:"lmtp"`
}

type LMTPProtocolConfig struct {
	// Greeting shown in the 220 banner. Default: "Dovecot ready."
	LoginGreeting string `koanf:"login_greeting"`
	// AddReceivedHeader prepends a Received: header to delivered messages. Default: true.
	AddReceivedHeader bool `koanf:"add_received_header"`
	// SaveToDetailMailbox delivers user+folder@domain to mailbox 'folder' instead of INBOX. Default: false.
	SaveToDetailMailbox bool `koanf:"save_to_detail_mailbox"`
	// HdrDeliveryAddress controls the Delivered-To header: none | final | original. Default: "final".
	HdrDeliveryAddress string `koanf:"hdr_delivery_address"`
	// VerboseReplies includes diagnostic details in error responses. Default: false.
	VerboseReplies bool `koanf:"verbose_replies"`
	// UserConcurrencyLimit is the max concurrent deliveries per user (0 = unlimited). Default: 0.
	UserConcurrencyLimit int `koanf:"user_concurrency_limit"`
	// ReadTimeout is the per-command read timeout in seconds. Default: 300.
	ReadTimeout int `koanf:"read_timeout"`
	// WriteTimeout is the per-command write timeout in seconds. Default: 300.
	WriteTimeout int `koanf:"write_timeout"`
	// ClientWorkarounds is a list of client compatibility workarounds.
	ClientWorkarounds []string `koanf:"client_workarounds"`
	// Proxy configures LMTP proxy mode (director → backend routing).
	Proxy LMTPProxyConfig `koanf:"proxy"`
}

// LMTPProxyConfig enables director-mode LMTP proxy: routes recipients to
// backend nodes via consistent hashing instead of delivering locally.
type LMTPProxyConfig struct {
	// Enabled activates proxy mode. When true, all recipients are routed to backends.
	Enabled bool `koanf:"enabled"`
	// Backends is the list of backend LMTP nodes.
	Backends []LMTPBackendEntry `koanf:"backends"`
	// Timeout is the per-backend connection+transaction timeout in seconds. Default: 125.
	Timeout int `koanf:"timeout"`
}

// LMTPBackendEntry is a single backend node for LMTP proxy routing.
type LMTPBackendEntry struct {
	Host string `koanf:"host"`
	Port int    `koanf:"port"` // default 24
}

type IMAPProtocolConfig struct {
	IdleNotifyInterval int    `koanf:"imap_idle_notify_interval"` // seconds; 0 = disabled
	MaxLineLength      int    `koanf:"imap_max_line_length"`      // bytes; 0 = unlimited
	IDSend             string `koanf:"imap_id_send"`              // ID pairs; * = default; empty = disabled
	LoginGreeting      string `koanf:"login_greeting"`
	LogoutFormat       string `koanf:"imap_logout_format"`
}

type POP3ProtocolConfig struct {
	NoFlagUpdates  bool   `koanf:"pop3_no_flag_updates"`
	ReuseXUIDL     bool   `koanf:"pop3_reuse_xuidl"`
	UIDLFormat     string `koanf:"pop3_uidl_format"`
	UIDLDuplicates string `koanf:"pop3_uidl_duplicates"` // allow | rename
	EnableLast     bool   `koanf:"pop3_enable_last"`
	DeleteType     string `koanf:"pop3_delete_type"` // expunge | flag
	DeletedFlag    string `koanf:"pop3_deleted_flag"`
}

type SMTPProtocolConfig struct {
	Hostname           string          `koanf:"hostname"`
	MaxMsgSize         int64           `koanf:"max_message_size"`
	MaxLineLength      int             `koanf:"max_line_length"`
	MaxRecipients      int             `koanf:"max_recipients"` // 0 = unlimited (Dovecot default)
	RecipientDelimiter string          `koanf:"recipient_delimiter"`
	Workarounds        []string        `koanf:"client_workarounds"` // whitespace-before-path | mailbox-for-path | implicit-auth-external
	Relay              SMTPRelayConfig `koanf:"relay"`
}

// SMTPRelayConfig mirrors Dovecot's submission_relay_* settings.
// Host must be non-empty to enable relaying; otherwise submission returns 451.
type SMTPRelayConfig struct {
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

type AuthConfig struct {
	Passdb []PassdbEntry `koanf:"passdb"`
}

type PassdbEntry struct {
	Driver string            `koanf:"driver"` // sqlite | mysql | postgres
	DSN    string            `koanf:"dsn"`
	Args   map[string]string `koanf:"args"`
}

type StorageConfig struct {
	Mailbox     string `koanf:"mailbox"`
	MaildirRoot string `koanf:"maildir_root"`
	Index       string `koanf:"index"`
	IndexDir    string `koanf:"index_dir"`
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
			},
			POP3: POP3ProtocolConfig{
				UIDLFormat:     "%u.%v",
				UIDLDuplicates: "rename",
				DeleteType:     "expunge",
			},
			SMTP: SMTPProtocolConfig{
				MaxMsgSize:         41943040,
				MaxLineLength:      4096,
				RecipientDelimiter: "+",
				Relay: SMTPRelayConfig{
					Port:           25,
					SSL:            "no",
					SSLVerify:      true,
					ConnectTimeout: 30,
					CommandTimeout: 300,
				},
			},
			LMTP: LMTPProtocolConfig{
				LoginGreeting:      "Dovecot ready.",
				AddReceivedHeader:  true,
				HdrDeliveryAddress: "final",
				ReadTimeout:        300,
				WriteTimeout:       300,
			},
		},
		Telemetry: TelemetryConfig{Listen: ":8080"},
		Log:       LogConfig{Level: "info"},
	}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	expandEnv(cfg)
	return cfg, nil
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
	expandSvcSSL(cfg.Services.IMAP)
	expandSvcSSL(cfg.Services.IMAPS)
	expandSvcSSL(cfg.Services.SMTP)
	expandSvcSSL(cfg.Services.Submission)
	expandSvcSSL(cfg.Services.Submissions)
	expandSvcSSL(cfg.Services.POP3)
	expandSvcSSL(cfg.Services.POP3S)
	cfg.Protocol.SMTP.Relay.Password = expand(cfg.Protocol.SMTP.Relay.Password)
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
