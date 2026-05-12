package config

import (
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config is the top-level yarilo configuration.
type Config struct {
	Mode string `koanf:"mode"` // proxy | director | backend | single

	IMAP      IMAPConfig      `koanf:"imap"`
	Auth      AuthConfig      `koanf:"auth"`
	Storage   StorageConfig   `koanf:"storage"`
	Telemetry TelemetryConfig `koanf:"telemetry"`
	Log       LogConfig       `koanf:"log"`
}

type IMAPConfig struct {
	Listen      string `koanf:"listen"`       // e.g. ":993"
	ListenPlain string `koanf:"listen_plain"` // e.g. ":143" for STARTTLS
	TLSCert     string `koanf:"tls_cert"`
	TLSKey      string `koanf:"tls_key"`
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
	Mailbox     string `koanf:"mailbox"`      // maildir | dbox | mdbox | obox
	MaildirRoot string `koanf:"maildir_root"` // e.g. /var/mail/vhosts
	Index       string `koanf:"index"`        // fileindex | sqlite | cassandra
	IndexDir    string `koanf:"index_dir"`    // for fileindex; defaults to maildir_root
}

type TelemetryConfig struct {
	Listen string `koanf:"listen"` // default :8080
}

type LogConfig struct {
	Level string `koanf:"level"` // debug | info | warn | error
}

// Load reads the YAML config file at path.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, err
	}
	cfg := &Config{
		Mode:      "single",
		IMAP:      IMAPConfig{Listen: ":993"},
		Telemetry: TelemetryConfig{Listen: ":8080"},
		Log:       LogConfig{Level: "info"},
	}
	if err := k.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
