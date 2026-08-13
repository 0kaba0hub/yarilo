package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

// The alias contract, in the four states a key can be in. The pair carries
// different values in the conflict row on purpose: a rename that resolves a
// disagreement by picking a winner changes behaviour on a config nobody edited,
// which is the failure the refusal exists to prevent.
func TestRotationTripleAliases(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  string
		wantSize string
		wantAge  int
	}{
		{
			name:     "canonical alone",
			body:     "storage:\n  mail_index_log_rotate_min_size: \"64k\"\n  mail_index_log_rotate_min_age: 60\n",
			wantSize: "64k",
			wantAge:  60,
		},
		{
			name:     "alias alone is adopted",
			body:     "storage:\n  index_log_compact_min_bytes: \"64k\"\n  index_log_compact_min_age_secs: 60\n",
			wantSize: "64k",
			wantAge:  60,
		},
		{
			name:     "both agreeing",
			body:     "storage:\n  mail_index_log_rotate_min_size: \"64k\"\n  index_log_compact_min_bytes: \"64k\"\n",
			wantSize: "64k",
		},
		{
			name:    "both disagreeing is refused",
			body:    "storage:\n  mail_index_log_rotate_min_size: \"64k\"\n  index_log_compact_min_bytes: \"128k\"\n",
			wantErr: "storage.mail_index_log_rotate_min_size",
		},
		{
			name:    "disagreement on the age arm is refused too",
			body:    "storage:\n  mail_index_log_rotate_min_age: 60\n  index_log_compact_min_age_secs: 120\n",
			wantErr: "storage.index_log_compact_min_age_secs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tc.body)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("both spellings disagreed and the config loaded anyway")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Storage.MailIndexLogRotateMinSizeRaw != tc.wantSize {
				t.Errorf("min size = %q, want %q", cfg.Storage.MailIndexLogRotateMinSizeRaw, tc.wantSize)
			}
			if cfg.Storage.MailIndexLogRotateMinAge != tc.wantAge {
				t.Errorf("min age = %d, want %d", cfg.Storage.MailIndexLogRotateMinAge, tc.wantAge)
			}
		})
	}
}

// A key set to zero is set. Inferring presence from the value instead of asking
// koanf would read this as "unset", silently adopt the alias, and turn rotation
// on for an operator who wrote 0 to turn it off.
func TestExplicitZeroIsNotTreatedAsUnset(t *testing.T) {
	cfg, err := loadYAML(t, "storage:\n  mail_index_log_rotate_min_size: \"0\"\n  index_log_compact_min_bytes: \"32k\"\n")
	if err == nil {
		t.Fatalf("a canonical 0 against an alias 32k loaded as %q", cfg.Storage.MailIndexLogRotateMinSizeRaw)
	}
}
