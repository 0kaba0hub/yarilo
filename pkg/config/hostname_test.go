package config

import (
	"os"
	"testing"
)

// One key names the installation, and it must never resolve to a literal.
//
// It did: nothing filled it, so the LHLO banner, the Received header and the
// synthesised Message-ID all fell back to the string "yarilo" — on every
// deployment, not one (#1506).
func TestHostnameDefaultsToThisHostNotToALiteral(t *testing.T) {
	cfg, err := loadYAML(t, "mode: single\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip("this machine has no hostname to compare against")
	}
	if cfg.Hostname != host {
		t.Errorf("hostname = %q, want this host's name %q", cfg.Hostname, host)
	}
	if cfg.Hostname == "yarilo" && host != "yarilo" {
		t.Error("hostname resolved to the literal the fallback used")
	}
}

// A configured value wins, and submission's own key overrides submission
// alone.
func TestSubmissionHostnameFallsBackToTheInstallationName(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantGlobal     string
		wantSubmission string
	}{
		{
			name:           "global alone serves both",
			body:           "hostname: mx.example.com\n",
			wantGlobal:     "mx.example.com",
			wantSubmission: "mx.example.com",
		},
		{
			name: "submission overrides submission only",
			body: "hostname: mx.example.com\nprotocol:\n  submission:\n    hostname: smtp.example.com\n",
			// The global keeps naming the host for LMTP, the banner and the
			// message id; only submission announces the other name.
			wantGlobal:     "mx.example.com",
			wantSubmission: "smtp.example.com",
		},
		{
			name:           "submission alone does not become the installation's name",
			body:           "hostname: mx.example.com\nprotocol:\n  submission:\n    hostname: \"\"\n",
			wantGlobal:     "mx.example.com",
			wantSubmission: "mx.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tt.body)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Hostname != tt.wantGlobal {
				t.Errorf("hostname = %q, want %q", cfg.Hostname, tt.wantGlobal)
			}
			if got := cfg.SubmissionHostname(); got != tt.wantSubmission {
				t.Errorf("SubmissionHostname() = %q, want %q", got, tt.wantSubmission)
			}
		})
	}
}
