package config

import (
	"strings"
	"testing"
)

// An owner-templated namespace (prefix carries %u) must be well-formed or fail
// startup, loudly. A silently-skipped new namespace kind is the #1087 failure
// from the other side; a templated namespace whose location does not resolve
// per owner opens every owner at one shared path (#544/B1).
func TestValidateOwnerTemplatedNamespace(t *testing.T) {
	cases := []struct {
		name    string
		ns      NamespaceConfig
		wantErr string // substring, "" = accepted
	}{
		{"well-formed", NamespaceConfig{Type: "shared", Prefix: "user/%u/", Separator: "/", Location: "maildir:%h/Maildir"}, ""},
		{"well-formed bare", NamespaceConfig{Type: "shared", Prefix: "user/%u", Separator: "/", Location: "maildir:%h"}, ""},
		{"not templated is untouched", NamespaceConfig{Type: "shared", Prefix: "Public/", Separator: "/", Location: "maildir:/srv/public"}, ""},
		{"templated, no location", NamespaceConfig{Type: "shared", Prefix: "user/%u/", Separator: "/"}, "no location"},
		{"templated, fixed location", NamespaceConfig{Type: "shared", Prefix: "user/%u/", Separator: "/", Location: "maildir:/srv/shared"}, "no per-owner variable"},
		{"templated, bad prefix shape", NamespaceConfig{Type: "shared", Prefix: "user/%u/mail/", Separator: "/", Location: "maildir:%h"}, "after the owner variable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNamespaceTypes([]NamespaceConfig{tc.ns})
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("accepted config rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("misconfigured templated namespace accepted; want error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}
