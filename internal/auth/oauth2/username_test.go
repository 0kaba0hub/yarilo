package oauth2

import (
	"errors"
	"testing"
)

func TestUsernameTemplate(t *testing.T) {
	tests := []struct {
		name     string
		template string
		user     string
		want     string
		wantErr  bool
	}{
		{"identity default", "", "Alice@Example.com", "Alice@Example.com", false},
		{"identity %u", "%u", "Alice@Example.com", "Alice@Example.com", false},
		{"identity %{user}", "%{user}", "Alice@Example.com", "Alice@Example.com", false},
		{"lowercase %Lu", "%Lu", "Alice@Example.com", "alice@example.com", false},
		{"localpart %n", "%n", "alice@example.com", "alice", false},
		{"localpart %Ln", "%Ln", "ALICE@example.com", "alice", false},
		{"localpart no @", "%n", "alice", "alice", false},
		{"domain %d", "%d", "alice@example.com", "example.com", false},
		{"domain %Ld", "%Ld", "alice@Example.COM", "example.com", false},
		{"domain no @", "%d", "alice", "", false},
		{"mixed %n@%d", "%n@%d", "alice@example.com", "alice@example.com", false},
		{"literal text", "prefix-%u-suffix", "alice", "prefix-alice-suffix", false},
		{"trailing %", "alice%", "x", "", true},
		{"unknown %X", "%X", "x", "", true},
		{"unterminated %{", "%{user", "x", "", true},
		{"unknown %{foo}", "%{foo}", "x", "", true},
		{"trailing %L", "%L", "x", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UsernameTemplate(tc.template, tc.user)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompareUsername(t *testing.T) {
	tests := []struct {
		name          string
		claimUsername string
		authzid       string
		template      string
		want          string
		wantErr       error
	}{
		{"empty authzid uses claim", "alice@example.com", "", "%u", "alice@example.com", nil},
		{"identity match", "alice@example.com", "alice@example.com", "%u", "alice@example.com", nil},
		{"identity mismatch", "alice@example.com", "bob@example.com", "%u", "", ErrUsernameMismatch},
		{"lowercase match", "alice@example.com", "Alice@Example.com", "%Lu", "alice@example.com", nil},
		{"lowercase mismatch", "Alice@example.com", "Alice@Example.com", "%Lu", "", ErrUsernameMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompareUsername(tc.claimUsername, tc.authzid, tc.template)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want wrap of %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckActive(t *testing.T) {
	tests := []struct {
		name      string
		claims    Claims
		attribute string
		value     string
		wantErr   bool
	}{
		{"no check configured", Claims{}, "", "", false},
		{"attr present any value", Claims{Extra: map[string]string{"enabled": "yes"}}, "enabled", "", false},
		{"attr missing", Claims{Extra: map[string]string{}}, "enabled", "yes", true},
		{"value match", Claims{Extra: map[string]string{"enabled": "true"}}, "enabled", "true", false},
		{"value mismatch", Claims{Extra: map[string]string{"enabled": "false"}}, "enabled", "true", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckActive(&tc.claims, tc.attribute, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error")
				}
				if err != nil && !errors.Is(err, ErrInactiveAccount) {
					t.Errorf("err = %v, want wrap of ErrInactiveAccount", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
