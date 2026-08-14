package config

import (
	"strings"
	"testing"
)

// Package 1 of the key review (#1286): seven storage keys take their reference
// spelling, and the pre-beta names keep working as aliases. Every pair is
// exercised in the four states a key can be in — canonical alone, alias alone,
// both agreeing, both disagreeing — because a rename that resolves a
// disagreement by picking a winner changes behaviour on a config nobody edited.
func TestStorageRenamesPackageOne(t *testing.T) {
	tests := []struct {
		name      string
		canonical string // "key: value" in the storage section
		alias     string
		// read returns what the config resolved to, so the row asserts the
		// value that reaches consumers rather than the presence of a field.
		read func(*Config) string
		want string
	}{
		{"mail_driver", `mail_driver: mdbox`, `mailbox: mdbox`,
			func(c *Config) string { return c.Storage.MailDriver }, "mdbox"},
		{"mail_home", `mail_home: "%d/%u"`, `mail_home_template: "%d/%u"`,
			func(c *Config) string { return c.Storage.MailHome }, "%d/%u"},
		{"mail_index_path", `mail_index_path: /idx/%d/%n`, `index_dir: /idx/%d/%n`,
			func(c *Config) string { return c.Storage.MailIndexPath }, "/idx/%d/%n"},
		{"mail_volatile_path", `mail_volatile_path: /run/v/%d/%n`, `volatile_dir: /run/v/%d/%n`,
			func(c *Config) string { return c.Storage.MailVolatilePath }, "/run/v/%d/%n"},
		{"mail_control_path", `mail_control_path: /ctl/%d/%n`, `control_dir: /ctl/%d/%n`,
			func(c *Config) string { return c.Storage.MailControlPath }, "/ctl/%d/%n"},
		{"mail_alt_path from alt_dir", `mail_alt_path: /cold/%d/%n`, `alt_dir: /cold/%d/%n`,
			func(c *Config) string { return c.Storage.MailAltPath }, "/cold/%d/%n"},
		{"mail_alt_path from mdbox_alt_storage_path", `mail_alt_path: /cold/%d/%n`, `mdbox_alt_storage_path: /cold/%d/%n`,
			func(c *Config) string { return c.Storage.MailAltPath }, "/cold/%d/%n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("canonical alone", func(t *testing.T) {
				cfg := mustLoadStorage(t, tc.canonical)
				if got := tc.read(cfg); got != tc.want {
					t.Errorf("read %q, want %q", got, tc.want)
				}
			})
			t.Run("alias alone is adopted", func(t *testing.T) {
				cfg := mustLoadStorage(t, tc.alias)
				if got := tc.read(cfg); got != tc.want {
					t.Errorf("read %q, want %q", got, tc.want)
				}
			})
			t.Run("both agreeing", func(t *testing.T) {
				cfg := mustLoadStorage(t, tc.canonical+"\n  "+tc.alias)
				if got := tc.read(cfg); got != tc.want {
					t.Errorf("read %q, want %q", got, tc.want)
				}
			})
			t.Run("both disagreeing is refused", func(t *testing.T) {
				other := strings.Replace(tc.alias, tc.want, tc.want+"-other", 1)
				if other == tc.alias {
					other = strings.Replace(tc.alias, "mdbox", "maildir", 1)
				}
				if _, err := loadStorage(t, tc.canonical+"\n  "+other); err == nil {
					t.Error("a disagreement between the two spellings loaded anyway")
				}
			})
		})
	}
}

// The NFC knob is a bool defaulting to true, so "unset" and "set to false" are
// only distinguishable by asking koanf. Adopting the alias because the
// canonical field happens to hold its default would turn normalisation back on
// for an operator who wrote false.
func TestNFCAliasCarriesFalse(t *testing.T) {
	cfg := mustLoadStorage(t, "mailbox_list_normalize_to_nfc: false")
	if cfg.Storage.MailboxListNormalizeNamesToNFC {
		t.Error("the alias set to false did not reach the canonical field")
	}
	cfg = mustLoadStorage(t, "mail_driver: maildir")
	if !cfg.Storage.MailboxListNormalizeNamesToNFC {
		t.Error("the default was lost when neither spelling was given")
	}
	if _, err := loadStorage(t, "mailbox_list_normalize_names_to_nfc: true\n  mailbox_list_normalize_to_nfc: false"); err == nil {
		t.Error("two spellings disagreeing about normalisation loaded anyway")
	}
}

// alt_dir and mdbox_alt_storage_path are two pre-beta names for ONE path. Set
// to different values they must be refused — and this is the row that fails if
// the second alias is judged against the file instead of against what the first
// one already adopted, which is how a later alias silently overwrites an
// earlier one.
func TestTwoAliasesOfOneKeyCannotDisagree(t *testing.T) {
	if _, err := loadStorage(t, "alt_dir: /cold/a\n  mdbox_alt_storage_path: /cold/b"); err == nil {
		t.Fatal("two spellings of the cold tier, set to different paths, loaded anyway")
	}
	cfg := mustLoadStorage(t, "alt_dir: /cold/a\n  mdbox_alt_storage_path: /cold/a")
	if cfg.Storage.MailAltPath != "/cold/a" {
		t.Errorf("agreeing spellings resolved to %q", cfg.Storage.MailAltPath)
	}
}

// The namespace location has a split spelling in 2.4 — mail_driver + mail_path
// — and both forms are accepted. Both given at once is a conflict, and half the
// pair is a configuration error rather than a default.
func TestNamespaceLocationSplitForm(t *testing.T) {
	tests := []struct {
		name    string
		ns      string
		want    string
		wantErr string
	}{
		{
			name: "split form becomes the location",
			ns:   "  - prefix: \"\"\n    type: personal\n    separator: \"/\"\n    inbox: true\n    mail_driver: mdbox\n    mail_path: /srv/%d/%n",
			want: "mdbox:/srv/%d/%n",
		},
		{
			name: "url form still works",
			ns:   "  - prefix: \"\"\n    type: personal\n    separator: \"/\"\n    inbox: true\n    location: \"mdbox:/srv/%d/%n\"",
			want: "mdbox:/srv/%d/%n",
		},
		{
			name:    "both forms at once",
			ns:      "  - prefix: \"\"\n    type: personal\n    separator: \"/\"\n    inbox: true\n    location: \"mdbox:/srv/%d/%n\"\n    mail_driver: mdbox\n    mail_path: /srv/%d/%n",
			wantErr: "two spellings",
		},
		{
			name:    "half the pair",
			ns:      "  - prefix: \"\"\n    type: personal\n    separator: \"/\"\n    inbox: true\n    mail_driver: mdbox",
			wantErr: "only one of",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, "namespaces:\n"+tc.ns+"\n")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("loaded anyway: %+v", cfg.Namespaces)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not say %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(cfg.Namespaces) != 1 || cfg.Namespaces[0].Location != tc.want {
				t.Errorf("location = %q, want %q", cfg.Namespaces[0].Location, tc.want)
			}
		})
	}
}

func loadStorage(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return loadYAML(t, "storage:\n  "+body+"\n")
}

func mustLoadStorage(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := loadStorage(t, body)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}
