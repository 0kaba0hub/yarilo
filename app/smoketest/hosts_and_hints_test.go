package main

import (
	"errors"
	"strings"
	"testing"
)

// One -host was dialled by three protocols that can live on different
// addresses, so in the reference deployment no value satisfied all of them and
// one row was unreachable whatever it was set to (#1311). Each protocol resolves
// its own flag now, falling back to -host.
func TestPerProtocolHostsFallBackToHost(t *testing.T) {
	restore := setFlags(map[*string]string{
		flagHost:            "shared.example",
		flagPOP3Host:        "",
		flagManageSieveHost: "",
		flagLMTPLoginHost:   "",
	})
	for _, tc := range []struct {
		name string
		got  func() string
	}{
		{"pop3", pop3Host},
		{"managesieve", manageSieveHost},
		{"lmtp-login", lmtpLoginHost},
	} {
		if got := tc.got(); got != "shared.example" {
			t.Errorf("%s host = %q, want the -host default", tc.name, got)
		}
	}
	restore()

	// And each overrides independently — the case a single flag could not
	// express: three protocols, three addresses.
	restore = setFlags(map[*string]string{
		flagHost:            "shared.example",
		flagPOP3Host:        "pop3.example",
		flagManageSieveHost: "sieve.example",
		flagLMTPLoginHost:   "lmtp.internal",
	})
	defer restore()

	for _, tc := range []struct {
		name, want string
		got        func() string
	}{
		{"pop3", "pop3.example", pop3Host},
		{"managesieve", "sieve.example", manageSieveHost},
		{"lmtp-login", "lmtp.internal", lmtpLoginHost},
	} {
		if got := tc.got(); got != tc.want {
			t.Errorf("%s host = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A missing credential is an unchecked surface, not a failure: every other
// under-configured row skips with what it needs, and this one turned a red gate
// on an absent token.
func TestDirectorRowSkipsWithoutAToken(t *testing.T) {
	tests := []struct {
		name      string
		api       string
		token     string
		wantRun   bool
		wantNeeds []string
	}{
		{
			name:      "no api and no token",
			wantNeeds: []string{"-director-api", "DIRECTOR_API_TOKEN"},
		},
		{
			name:      "api without a token",
			api:       "http://director:9103",
			wantNeeds: []string{"-director-api-token", "DIRECTOR_API_TOKEN"},
		},
		{
			name:    "api and token",
			api:     "http://director:9103",
			token:   "t0ken",
			wantRun: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restore := setFlags(map[*string]string{
				flagDirectorAPI:      tc.api,
				flagDirectorAPIToken: tc.token,
			})
			defer restore()
			t.Setenv("DIRECTOR_API_TOKEN", "")
			t.Setenv("YARILO_ADMIN_TOKEN", "")

			var row *check
			for _, c := range register() {
				if c.area == "director" {
					row = &c
					break
				}
			}
			if row == nil {
				t.Fatal("no director row registered")
			}
			if tc.wantRun {
				if row.skip != "" {
					t.Errorf("a fully configured row skipped: %q", row.skip)
				}
				return
			}
			if row.skip == "" {
				t.Fatal("an under-configured row is runnable; it would fail the gate on a missing credential")
			}
			for _, want := range tc.wantNeeds {
				if !strings.Contains(row.skip, want) {
					t.Errorf("skip %q does not name %q", row.skip, want)
				}
			}
		})
	}
}

// The three transport failures this row produces each have a flag that fixes
// them, and the error text never said so — three QA runs, one per error.
func TestBackendAPITransportErrorsNameTheirFlag(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want string
	}{
		{"mTLS handshake", "remote error: tls: certificate required", "-backend-api-cert"},
		{"plain http against TLS", "server gave HTTP response to HTTPS client: HTTP request to an HTTPS server", "https://"},
		{"untrusted certificate", "x509: certificate signed by unknown authority", "-backend-api-ca"},
		{"wrong service name", "dial tcp: lookup yarilo-backend-api: no such host", "yarilo-backend:9105"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := explainBackendAPITransport(errors.New(tc.err))
			if !strings.Contains(got.Error(), tc.want) {
				t.Errorf("explanation %q does not point at %q", got, tc.want)
			}
			if !strings.Contains(got.Error(), tc.err) {
				t.Errorf("explanation dropped the original error: %q", got)
			}
		})
	}

	// An error nobody has a hint for is passed through unchanged rather than
	// decorated with a guess.
	other := errors.New("connection reset by peer")
	if got := explainBackendAPITransport(other); got.Error() != other.Error() {
		t.Errorf("an unexplained error was rewritten: %q", got)
	}
	if explainBackendAPITransport(nil) != nil {
		t.Error("nil became an error")
	}
}
