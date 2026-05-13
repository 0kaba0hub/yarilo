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
	Mode string `koanf:"mode"` // proxy | director | backend | single

	IMAP      IMAPConfig      `koanf:"imap"`
	SMTP      SMTPConfig      `koanf:"smtp"`
	DKIM      DKIMConfig      `koanf:"dkim"`
	SPF       SPFConfig       `koanf:"spf"`
	DMARC     DMARCConfig     `koanf:"dmarc"`
	Auth      AuthConfig      `koanf:"auth"`
	Storage   StorageConfig   `koanf:"storage"`
	Telemetry TelemetryConfig `koanf:"telemetry"`
	Log       LogConfig       `koanf:"log"`
}

type IMAPConfig struct {
	Listen           string `koanf:"listen"`
	ListenPlain      string `koanf:"listen_plain"`
	TLSCert          string `koanf:"tls_cert"`
	TLSKey           string `koanf:"tls_key"`
	TLSAltCert       string `koanf:"tls_alt_cert"`              // optional secondary cert (e.g. ECDSA)
	TLSAltKey        string `koanf:"tls_alt_key"`               // optional secondary key
	TLSMinVersion    string `koanf:"tls_min_protocol"`          // TLS1.2 | TLS1.3
	TLSPreferServer  bool   `koanf:"tls_prefer_server_ciphers"` // server cipher order
	ProxyProtocol    bool   `koanf:"proxy_protocol"`
	HAProxyTimeout   int    `koanf:"haproxy_timeout"`        // seconds, default 3
	DisablePlainAuth bool   `koanf:"disable_plaintext_auth"` // reject auth without TLS
}

type SMTPConfig struct {
	ListenMX           string         `koanf:"listen_mx"`
	ListenSubmit       string         `koanf:"listen_submit"`
	Hostname           string         `koanf:"hostname"`
	MaxMsgSize         int64          `koanf:"max_message_size"`
	TLSCert            string         `koanf:"tls_cert"`
	TLSKey             string         `koanf:"tls_key"`
	TLSAltCert         string         `koanf:"tls_alt_cert"`              // optional secondary cert (e.g. ECDSA)
	TLSAltKey          string         `koanf:"tls_alt_key"`               // optional secondary key
	TLSMinVersion      string         `koanf:"tls_min_protocol"`          // TLS1.2 | TLS1.3
	TLSPreferServer    bool           `koanf:"tls_prefer_server_ciphers"` // server cipher order
	ProxyProtocol      bool           `koanf:"proxy_protocol"`            // HAProxy PROXY protocol on both listeners
	HAProxyTrustedNets []string       `koanf:"haproxy_trusted_nets"`      // CIDRs allowed to send PROXY header
	HAProxyTimeout     int            `koanf:"haproxy_timeout"`           // seconds, default 3
	XClient            bool           `koanf:"xclient"`                   // advertise and handle XCLIENT on MX port
	XClientTrustedNets []string       `koanf:"xclient_trusted_nets"`      // CIDRs allowed to send XCLIENT
	DisablePlainAuth   bool           `koanf:"disable_plaintext_auth"`    // reject submission AUTH without TLS
	MaxLineLength      int            `koanf:"max_line_length"`           // SMTP command line limit, default 4096
	RecipientDelimiter string         `koanf:"recipient_delimiter"`       // subaddress delimiter, default "+"
	Milters            []MilterConfig `koanf:"milters"`
}

type MilterConfig struct {
	Socket  string `koanf:"socket"`  // unix:/path or tcp:host:port
	Timeout int    `koanf:"timeout"` // seconds, default 30
}

type DKIMConfig struct {
	Verify          bool           `koanf:"verify"`
	Sign            bool           `koanf:"sign"`
	Selector        string         `koanf:"selector"`
	SignHeaders     []string       `koanf:"sign_headers"`
	OversignHeaders []string       `koanf:"oversign_headers"`
	Keys            DKIMKeysConfig `koanf:"keys"`
}

type DKIMKeysConfig struct {
	Backend string            `koanf:"backend"` // static | dynamic
	Static  map[string]string `koanf:"static"`  // domain → PEM file path
	Dynamic DKIMDynamicConfig `koanf:"dynamic"`
}

// DKIMDynamicConfig holds DB connection info for DKIM key lookup.
// DSN supports ${ENV_VAR} substitution.
type DKIMDynamicConfig struct {
	Driver   string `koanf:"driver"`    // sqlite | mysql | postgres
	DSN      string `koanf:"dsn"`       // supports ${ENV_VAR}
	Query    string `koanf:"query"`     // must return single column: private_key PEM
	CacheTTL int    `koanf:"cache_ttl"` // seconds, default 300
}

type SPFConfig struct {
	Enabled bool `koanf:"enabled"`
}

type DMARCConfig struct {
	Enabled bool `koanf:"enabled"`
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

// Load reads the YAML config file at path.
// All string values support ${ENV_VAR} substitution.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, err
	}
	defaultTrustedNets := []string{"127.0.0.1/32", "10.0.0.0/8"}
	cfg := &Config{
		Mode: "single",
		IMAP: IMAPConfig{
			Listen:           ":993",
			TLSMinVersion:    "TLS1.2",
			HAProxyTimeout:   3,
			DisablePlainAuth: true,
		},
		SMTP: SMTPConfig{
			ListenMX:           ":25",
			ListenSubmit:       ":587",
			MaxMsgSize:         41943040,
			TLSMinVersion:      "TLS1.2",
			HAProxyTrustedNets: defaultTrustedNets,
			HAProxyTimeout:     3,
			XClientTrustedNets: defaultTrustedNets,
			DisablePlainAuth:   true,
			MaxLineLength:      4096,
			RecipientDelimiter: "+",
		},
		DKIM:      DKIMConfig{Selector: "mail", SignHeaders: defaultSignHeaders, OversignHeaders: defaultOversignHeaders},
		Telemetry: TelemetryConfig{Listen: ":8080"},
		Log:       LogConfig{Level: "info"},
	}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	expandEnv(cfg)
	return cfg, nil
}

var defaultSignHeaders = []string{
	"From", "To", "Subject", "Date", "Message-ID", "Content-Type",
}

var defaultOversignHeaders = []string{"From"}

// expandEnv substitutes ${ENV_VAR} in all string fields that may contain secrets.
func expandEnv(cfg *Config) {
	cfg.SMTP.TLSCert = expand(cfg.SMTP.TLSCert)
	cfg.SMTP.TLSKey = expand(cfg.SMTP.TLSKey)
	cfg.SMTP.TLSAltCert = expand(cfg.SMTP.TLSAltCert)
	cfg.SMTP.TLSAltKey = expand(cfg.SMTP.TLSAltKey)
	cfg.IMAP.TLSCert = expand(cfg.IMAP.TLSCert)
	cfg.IMAP.TLSKey = expand(cfg.IMAP.TLSKey)
	cfg.IMAP.TLSAltCert = expand(cfg.IMAP.TLSAltCert)
	cfg.IMAP.TLSAltKey = expand(cfg.IMAP.TLSAltKey)
	cfg.DKIM.Keys.Dynamic.DSN = expand(cfg.DKIM.Keys.Dynamic.DSN)
	for i := range cfg.Auth.Passdb {
		cfg.Auth.Passdb[i].DSN = expand(cfg.Auth.Passdb[i].DSN)
	}
}

// BuildTLSConfig constructs a *tls.Config from cert/key paths and TLS settings.
// altCert/altKey are optional (dual-cert: RSA primary + ECDSA secondary).
// minVersion: "TLS1.2" (default) or "TLS1.3". preferServer controls cipher ordering.
func BuildTLSConfig(certFile, keyFile, altCert, altKey, minVersion string, preferServer bool) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: load cert %q: %w", certFile, err)
	}
	certs := []tls.Certificate{cert}
	if altCert != "" && altKey != "" {
		alt, err := tls.LoadX509KeyPair(altCert, altKey)
		if err != nil {
			return nil, fmt.Errorf("tls: load alt cert %q: %w", altCert, err)
		}
		certs = append(certs, alt)
	}
	cfg := &tls.Config{
		Certificates:             certs,
		PreferServerCipherSuites: preferServer,
		MinVersion:               tls.VersionTLS12,
	}
	if minVersion == "TLS1.3" {
		cfg.MinVersion = tls.VersionTLS13
	}
	return cfg, nil
}

func expand(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return os.ExpandEnv(s)
}
