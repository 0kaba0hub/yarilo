package main

import (
	"net/http"
	"testing"
)

// TestNewAdminClient covers the #954 client selection: no TLS inputs → plain
// DefaultClient; partial/mTLS inputs go through mtls.ClientConfig, which requires
// a full cert set and a ServerName (so a misconfig fails loudly instead of
// silently falling back to plain HTTP).
func TestNewAdminClient(t *testing.T) {
	t.Run("no tls inputs uses the default client", func(t *testing.T) {
		c, err := newAdminClient("", "", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c != http.DefaultClient {
			t.Fatalf("want http.DefaultClient, got %p", c)
		}
	})

	t.Run("server name without a cert errors (mTLS needs the full set)", func(t *testing.T) {
		if _, err := newAdminClient("", "", "", "yarilo-internal"); err == nil {
			t.Fatal("want error for ServerName without cert/key/ca, got nil")
		}
	})

	t.Run("ca without server name errors (pinned name required)", func(t *testing.T) {
		if _, err := newAdminClient("", "", "/nonexistent/ca.crt", ""); err == nil {
			t.Fatal("want error for missing ServerName, got nil")
		}
	})
}

// TestAdminScheme pins the scheme decision used for the director-resolved
// per-user pod URL (#954).
func TestAdminScheme(t *testing.T) {
	oldCA, oldCert, oldSN := tlsCA, tlsCert, tlsServerName
	defer func() { tlsCA, tlsCert, tlsServerName = oldCA, oldCert, oldSN }()

	tlsCA, tlsCert, tlsServerName = "", "", ""
	if got := adminScheme(); got != "http" {
		t.Fatalf("no tls → %q, want http", got)
	}
	tlsServerName = "yarilo-internal"
	if got := adminScheme(); got != "https" {
		t.Fatalf("tls set → %q, want https", got)
	}
}
