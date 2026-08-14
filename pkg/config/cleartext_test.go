package config

import (
	"strings"
	"testing"
)

// The one dangerous row of the key review (#1286, package 4): 2.4 renamed
// disable_plaintext_auth to auth_allow_cleartext and INVERTED its sense. A
// mechanical alias would copy the value across and turn an operator's security
// setting into its opposite on a config nobody edited.
//
// The row that matters is the equivalence: the two spellings, written with
// opposite values, must produce the same effective policy.
func TestBothSpellingsProduceTheSamePolicy(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantCleartext bool
	}{
		{
			name:          "pre-beta spelling, cleartext disabled",
			body:          "services:\n  imap:\n    listen: \":143\"\n    disable_plaintext_auth: true\n",
			wantCleartext: false,
		},
		{
			name:          "canonical spelling, the same policy",
			body:          "services:\n  imap:\n    listen: \":143\"\n    auth_allow_cleartext: false\n",
			wantCleartext: false,
		},
		{
			name:          "pre-beta spelling, cleartext allowed",
			body:          "services:\n  imap:\n    listen: \":143\"\n    disable_plaintext_auth: false\n",
			wantCleartext: true,
		},
		{
			name:          "canonical spelling, the same permissive policy",
			body:          "services:\n  imap:\n    listen: \":143\"\n    auth_allow_cleartext: true\n",
			wantCleartext: true,
		},
		{
			// Unset means what an unset disable_plaintext_auth meant, or the
			// rename would change behaviour for every config that never set it.
			name:          "neither spelling keeps the old unset behaviour",
			body:          "services:\n  imap:\n    listen: \":143\"\n",
			wantCleartext: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tc.body)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.Services.IMAP.CleartextAllowed(); got != tc.wantCleartext {
				t.Errorf("CleartextAllowed() = %v, want %v", got, tc.wantCleartext)
			}
			// The login servers read the inverse; it must agree by construction.
			if cfg.Services.IMAP.PlainAuthDisabled() == cfg.Services.IMAP.CleartextAllowed() {
				t.Error("the two readings of one policy do not oppose each other")
			}
		})
	}
}

// Both spellings at once is refused even when they agree — the property that
// separates this pair from an ordinary alias, where agreement is accepted.
// An operator carrying both is one edit away from meaning the opposite of what
// they wrote.
func TestBothSpellingsAtOnceRefuseStartup(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "disagreeing",
			body: "services:\n  imap:\n    listen: \":143\"\n    auth_allow_cleartext: true\n    disable_plaintext_auth: true\n",
		},
		{
			name: "agreeing, which is still two spellings of one setting",
			body: "services:\n  imap:\n    listen: \":143\"\n    auth_allow_cleartext: false\n    disable_plaintext_auth: true\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.body)
			if err == nil {
				t.Fatal("a config carrying both spellings loaded anyway")
			}
			for _, want := range []string{"auth_allow_cleartext", "disable_plaintext_auth", "opposite senses"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
		})
	}
}

// The managesieve duplicate is folded onto the sieve key it duplicated: one
// limit, two spellings in two sections, and the sieve one stays (#1286).
func TestManageSieveScriptSizeDuplicateFoldsOntoSieve(t *testing.T) {
	cfg, err := loadYAML(t, "protocol:\n  managesieve:\n    max_script_size: 32768\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sieve.MaxScriptSize != 32768 {
		t.Errorf("the duplicate did not reach sieve_max_script_size: %d", cfg.Sieve.MaxScriptSize)
	}

	// Both set to different values is a disagreement about one limit, refused
	// like any other pair rather than resolved by which section wins.
	if _, err := loadYAML(t, "sieve:\n  sieve_max_script_size: 65536\nprotocol:\n  managesieve:\n    max_script_size: 32768\n"); err == nil {
		t.Error("two values for one limit loaded anyway")
	}
}
