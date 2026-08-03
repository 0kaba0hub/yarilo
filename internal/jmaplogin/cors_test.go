package jmaplogin

import (
	"context"
	"net/http"
	"testing"
)

func req(t *testing.T, method, base, origin, authz string) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), method, base+"/.well-known/jmap", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	resp, err := keepAliveClient().Do(r)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// An endpoint any page can call with the user's credentials is an
// account-takeover surface, so cross-origin access is off unless configured.
func TestCORSDefaultsToDeny(t *testing.T) {
	base := startProxy(t, Options{})

	pre := req(t, http.MethodOptions, base, "https://evil.example", "")
	defer pre.Body.Close() //nolint:errcheck
	if pre.StatusCode != http.StatusForbidden {
		t.Errorf("preflight status = %d, want 403", pre.StatusCode)
	}
	if got := pre.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("a denied preflight advertised %q", got)
	}

	real := req(t, http.MethodGet, base, "https://evil.example", basic("u1", "pw"))
	defer real.Body.Close() //nolint:errcheck
	if real.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin request status = %d, want 403", real.StatusCode)
	}
}

func TestCORSAllowsAConfiguredOrigin(t *testing.T) {
	const origin = "https://mail.example.com"
	base := startProxy(t, Options{CORSAllowOrigins: []string{origin}})

	pre := req(t, http.MethodOptions, base, origin, "")
	defer pre.Body.Close() //nolint:errcheck
	if pre.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", pre.StatusCode)
	}
	if got := pre.Header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("allow-origin = %q, want %q", got, origin)
	}
	if got := pre.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("a named origin must be offered credentials, got %q", got)
	}
	if pre.Header.Get("Access-Control-Allow-Headers") == "" {
		t.Error("preflight named no allowed headers")
	}

	real := req(t, http.MethodGet, base, origin, basic("u1", "pw"))
	defer real.Body.Close() //nolint:errcheck
	if real.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", real.StatusCode)
	}
	if got := real.Header.Get("Vary"); got == "" {
		t.Error("a response varying by Origin must say so, or a cache serves it to another origin")
	}
}

// Matching is exact: prefix or suffix matching is how an allow-list quietly
// becomes a wildcard.
func TestCORSMatchesExactly(t *testing.T) {
	c := newCORS([]string{"https://mail.example.com"})
	tests := []struct {
		origin string
		want   bool
	}{
		{"https://mail.example.com", true},
		{"https://mail.example.com/", true},
		{"https://mail.example.com.evil.test", false},
		{"https://evil.test/https://mail.example.com", false},
		{"http://mail.example.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := c.allows(tt.origin); got != tt.want {
			t.Errorf("allows(%q) = %v, want %v", tt.origin, got, tt.want)
		}
	}
}

// A same-origin client sends no Origin and must be unaffected by any of this.
func TestCORSIgnoresRequestsWithoutOrigin(t *testing.T) {
	base := startProxy(t, Options{})
	resp := req(t, http.MethodGet, base, "", basic("u1", "pw"))
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
