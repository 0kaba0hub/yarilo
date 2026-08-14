package config

import (
	"fmt"
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

// A refusal has to name the key the operator wrote. Before the rename the
// template check reported the pre-beta spelling whatever the config said, so
// someone who wrote mail_index_path was sent looking for index_dir — a key not
// in their file. The value below is a template naming a variable that does not
// exist, which is what ValidatePathTemplates refuses.
func TestTemplateRefusalNamesTheCanonicalKey(t *testing.T) {
	tests := []struct {
		field func(*StorageConfig)
		want  string
	}{
		{func(s *StorageConfig) { s.MailIndexPath = "/idx/%{nosuchvar}" }, "mail_index_path"},
		{func(s *StorageConfig) { s.MailControlPath = "/ctl/%{nosuchvar}" }, "mail_control_path"},
		{func(s *StorageConfig) { s.MailVolatilePath = "/run/%{nosuchvar}" }, "mail_volatile_path"},
		{func(s *StorageConfig) { s.MailAltPath = "/cold/%{nosuchvar}" }, "mail_alt_path"},
		{func(s *StorageConfig) { s.MailHome = "/home/%{nosuchvar}" }, "mail_home"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			sc := &StorageConfig{}
			tc.field(sc)
			err := ValidatePathTemplates(sc)
			if err == nil {
				t.Fatalf("a template naming an unknown variable was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
}

// Package 2a of the key review (#1286): the infrastructure sections take the
// flat prefixed spellings the reference uses. Same four states per pair as
// package 1 — the alias must carry the value, and a disagreement must refuse.
func TestInfraRenamesPackageTwoA(t *testing.T) {
	tests := []struct {
		name    string
		section string // YAML path, one line per level
		indent  string
		canon   string // leaf line, canonical
		alias   string // leaf line, pre-beta
		read    func(*Config) string
		want    string
	}{
		{"ssl_server_cert_file", "general:\n  ssl:", "    ",
			"ssl_server_cert_file: /c.pem", "tls_cert: /c.pem",
			func(c *Config) string { return c.General.SSL.SSLServerCert }, "/c.pem"},
		{"ssl_min_protocol", "general:\n  ssl:", "    ",
			"ssl_min_protocol: TLS1.3", "tls_min_version: TLS1.3",
			func(c *Config) string { return c.General.SSL.SSLMinProtocol }, "TLS1.3"},
		{"acl_globals_only", "acl:", "  ",
			"acl_globals_only: true", "globals_only: true",
			func(c *Config) string { return fmt.Sprint(c.ACL.GlobalsOnly) }, "true"},
		{"auth_cache_size", "auth:\n  cache:", "    ",
			"auth_cache_size: 64M", "cache_size: 64M",
			func(c *Config) string { return c.Auth.Cache.CacheSize }, "64M"},
		{"auth_policy_server_url", "auth:\n  policy:", "    ",
			"auth_policy_server_url: https://p/x", "url: https://p/x",
			func(c *Config) string { return c.Auth.Policy.URL }, "https://p/x"},
		{"auth_master_user_separator", "auth:\n  master_users:", "    ",
			"auth_master_user_separator: \"*\"", "separator: \"*\"",
			func(c *Config) string { return c.Auth.MasterUsers.Separator }, "*"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := func(leaves ...string) string {
				out := tc.section
				for _, l := range leaves {
					out += "\n" + tc.indent + l
				}
				return out + "\n"
			}
			for _, form := range []struct {
				label  string
				leaves []string
			}{
				{"canonical alone", []string{tc.canon}},
				{"alias alone", []string{tc.alias}},
				{"both agreeing", []string{tc.canon, tc.alias}},
			} {
				cfg, err := loadYAML(t, body(form.leaves...))
				if err != nil {
					t.Fatalf("%s: load: %v", form.label, err)
				}
				if got := tc.read(cfg); got != tc.want {
					t.Errorf("%s: read %q, want %q", form.label, got, tc.want)
				}
			}
			other := strings.Replace(tc.alias, tc.want, "other", 1)
			if _, err := loadYAML(t, body(tc.canon, other)); err == nil {
				t.Error("the two spellings disagreeing loaded anyway")
			}
		})
	}
}

// A list-valued knob compares as a value, not by identity: haproxy's trusted
// networks are the one alias in this package whose type is a slice, and a
// pointer comparison would call two equal lists a conflict.
func TestListValuedAliasComparesAsAValue(t *testing.T) {
	both := "general:\n  haproxy:\n    haproxy_trusted_networks: [\"10.0.0.0/8\"]\n    trusted_nets: [\"10.0.0.0/8\"]\n"
	cfg, err := loadYAML(t, both)
	if err != nil {
		t.Fatalf("two equal lists were refused: %v", err)
	}
	if len(cfg.General.HAProxy.HAProxyTrustedNetworks) != 1 {
		t.Errorf("networks = %v", cfg.General.HAProxy.HAProxyTrustedNetworks)
	}
	differing := "general:\n  haproxy:\n    haproxy_trusted_networks: [\"10.0.0.0/8\"]\n    trusted_nets: [\"192.168.0.0/16\"]\n"
	if _, err := loadYAML(t, differing); err == nil {
		t.Error("two different lists loaded anyway")
	}
}

// A per-service ssl override is the same block type as general.ssl, so it
// carries the same pre-beta spellings — and the fixed general.ssl paths do not
// reach it. An override written the old way used to fill a field nobody
// adopted, and the service lost its certificate: set, and doing nothing.
func TestPerServiceSSLOverrideAcceptsBothSpellings(t *testing.T) {
	const svc = "services:\n  imaps:\n    listen: \":993\"\n    ssl:\n"

	cfg, err := loadYAML(t, svc+"      tls_cert: /svc.pem\n      tls_key: /svc.key\n")
	if err != nil {
		t.Fatalf("old spelling: %v", err)
	}
	if got := cfg.Services.IMAPS.SSL.SSLServerCert; got != "/svc.pem" {
		t.Errorf("certificate from the pre-beta spelling = %q, want /svc.pem", got)
	}
	if got := cfg.Services.IMAPS.SSL.SSLServerKey; got != "/svc.key" {
		t.Errorf("key from the pre-beta spelling = %q, want /svc.key", got)
	}

	cfg, err = loadYAML(t, svc+"      ssl_server_cert_file: /svc.pem\n")
	if err != nil {
		t.Fatalf("canonical spelling: %v", err)
	}
	if got := cfg.Services.IMAPS.SSL.SSLServerCert; got != "/svc.pem" {
		t.Errorf("certificate from the canonical spelling = %q", got)
	}

	if _, err := loadYAML(t, svc+"      ssl_server_cert_file: /a.pem\n      tls_cert: /b.pem\n"); err == nil {
		t.Error("a service override with two spellings at different values loaded anyway")
	}
}

// auth.token is ours — the reference has no token store — so package 2 must not
// have touched it. The row reads the value back rather than the tag, because
// the defect was a key that parsed into a field nobody adopted and silently
// fell back to the default.
func TestOursOnlyKeysAreUntouched(t *testing.T) {
	cfg, err := loadYAML(t, "auth:\n  token:\n    ttl_seconds: 120\n    backend: memory\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.Token.TTLSeconds != 120 {
		t.Errorf("auth.token.ttl_seconds read as %d, want 120", cfg.Auth.Token.TTLSeconds)
	}
}

// Package 2b: the protocol and provider sections. The rows below are the ones
// whose shape differs from a plain scalar rename — the repeated key name, the
// list entry, the size-valued knob and the one the reference spells against the
// pattern. The rest are covered mechanically by the alias-coverage check.
func TestProtocolRenamesPackageTwoB(t *testing.T) {
	t.Run("the same key name in three sections stays three keys", func(t *testing.T) {
		cfg, err := loadYAML(t, "protocol:\n"+
			"  imap:\n    client_workarounds: [\"delay-newmail\"]\n"+
			"  lmtp:\n    client_workarounds: [\"whitespace-before-path\"]\n"+
			"  submission:\n    client_workarounds: [\"mailbox-for-path\"]\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got := cfg.Protocol.IMAP.ClientWorkarounds; len(got) != 1 || got[0] != "delay-newmail" {
			t.Errorf("imap workarounds = %v", got)
		}
		if got := cfg.Protocol.LMTP.ClientWorkarounds; len(got) != 1 || got[0] != "whitespace-before-path" {
			t.Errorf("lmtp workarounds = %v", got)
		}
		if got := cfg.Protocol.Submission.Workarounds; len(got) != 1 || got[0] != "mailbox-for-path" {
			t.Errorf("submission workarounds = %v", got)
		}
	})

	t.Run("a list entry adopts per entry, not per section", func(t *testing.T) {
		cfg, err := loadYAML(t, "auth:\n  passdb:\n"+
			"    - driver: sql\n      password_query: \"SELECT 1\"\n"+
			"    - driver: passwd-file\n      passwd_file: /etc/yarilo/users\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(cfg.Auth.Passdb) != 2 {
			t.Fatalf("%d passdb entries", len(cfg.Auth.Passdb))
		}
		if got := cfg.Auth.Passdb[0].PasswordQuery; got != "SELECT 1" {
			t.Errorf("first entry query = %q", got)
		}
		if got := cfg.Auth.Passdb[1].PasswdFile; got != "/etc/yarilo/users" {
			t.Errorf("second entry passwd file = %q", got)
		}
	})

	t.Run("the size-valued knob is parsed after adoption", func(t *testing.T) {
		cfg, err := loadYAML(t, "quota:\n  quota_grace: 20M\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		// Read the way the consumers read it: the canonical field has to hold
		// the string, or the size parses to zero and the grace silently
		// disappears.
		if cfg.Quota.Grace != "20M" {
			t.Errorf("grace = %q, want 20M through the alias", cfg.Quota.Grace)
		}
	})

	t.Run("passwd_file_path carries no passdb prefix", func(t *testing.T) {
		cfg, err := loadYAML(t, "auth:\n  passdb:\n    - driver: passwd-file\n      passwd_file_path: /etc/yarilo/users\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got := cfg.Auth.Passdb[0].PasswdFile; got != "/etc/yarilo/users" {
			t.Errorf("canonical spelling read as %q", got)
		}
	})

	t.Run("both spellings disagreeing refuse startup", func(t *testing.T) {
		_, err := loadYAML(t, "protocol:\n  lmtp:\n    lmtp_verbose_replies: true\n    verbose_replies: false\n")
		if err == nil {
			t.Error("two spellings of verbose replies loaded anyway")
		}
	})
}

// The oauth2 section follows one rule: every key carries the section prefix.
// Where the reference has a key the reference spelling is used; where it has
// none the prefixed name is ours, said so in the struct. Half the section
// prefixed and half bare was the alternative, and it is the inconsistency the
// package exists to end — so the rows cover both kinds.
func TestOAuth2SectionIsPrefixedThroughout(t *testing.T) {
	tests := []struct {
		name string
		leaf string // pre-beta spelling inside the entry
		read func(*Config) string
		want string
		kind string // reference | ours
	}{
		{"scopes", "scopes: [\"openid\"]", func(c *Config) string { return firstOf(c.Auth.OAuth2[0].Scopes) }, "openid", "reference"},
		{"username_attribute", "username_attribute: email", func(c *Config) string { return c.Auth.OAuth2[0].UsernameAttribute }, "email", "reference"},
		{"active_attribute", "active_attribute: active", func(c *Config) string { return c.Auth.OAuth2[0].ActiveAttribute }, "active", "reference"},
		{"active_value", "active_value: yes", func(c *Config) string { return c.Auth.OAuth2[0].ActiveValue }, "yes", "reference"},
		{"extra_fields", "extra_fields: [\"groups\"]", func(c *Config) string { return firstOf(c.Auth.OAuth2[0].ExtraFields) }, "groups", "reference"},
		{"audience", "audience: yarilo", func(c *Config) string { return c.Auth.OAuth2[0].Audience }, "yarilo", "ours"},
		{"jwks_url", "jwks_url: https://idp/jwks", func(c *Config) string { return c.Auth.OAuth2[0].JWKSURL }, "https://idp/jwks", "ours"},
	}

	for _, tc := range tests {
		t.Run(tc.name+" ("+tc.kind+")", func(t *testing.T) {
			cfg, err := loadYAML(t, "auth:\n  oauth2:\n    - mode: local_jwt\n      "+tc.leaf+"\n")
			if err != nil {
				t.Fatalf("pre-beta spelling: %v", err)
			}
			if got := tc.read(cfg); got != tc.want {
				t.Errorf("through the alias: %q, want %q", got, tc.want)
			}
		})
	}

	// And the canonical spellings reach the same fields.
	cfg, err := loadYAML(t, "auth:\n  oauth2:\n    - oauth2_mode: local_jwt\n"+
		"      oauth2_scope: [\"openid\"]\n      oauth2_audience: yarilo\n      oauth2_jwks_url: https://idp/jwks\n")
	if err != nil {
		t.Fatalf("canonical spellings: %v", err)
	}
	e := cfg.Auth.OAuth2[0]
	if firstOf(e.Scopes) != "openid" || e.Audience != "yarilo" || e.JWKSURL != "https://idp/jwks" || string(e.Mode) != "local_jwt" {
		t.Errorf("canonical entry read as %+v", e)
	}
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
