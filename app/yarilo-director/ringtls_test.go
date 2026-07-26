package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRingTLSMisconfigured(t *testing.T) {
	cases := []struct {
		name       string
		tlsEnabled bool
		peers      []string
		serverName string
		want       bool
	}{
		{"tls off", false, []string{"seed:9102"}, "", false},
		{"no peers (singleton)", true, nil, "", false},
		{"peers + name set", true, []string{"seed:9102"}, "rel-director-ring", false},
		{"peers + empty name => loud fail", true, []string{"seed:9102"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ringTLSMisconfigured(tc.tlsEnabled, tc.peers, tc.serverName); got != tc.want {
				t.Fatalf("ringTLSMisconfigured(%v,%v,%q) = %v, want %v", tc.tlsEnabled, tc.peers, tc.serverName, got, tc.want)
			}
		})
	}
}

func writeTestCert(t *testing.T, dnsNames ...string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCertHasSAN(t *testing.T) {
	path := writeTestCert(t, "rel-director-ring", "rel-director")
	if !certHasSAN(path, "rel-director-ring") {
		t.Error("expected SAN rel-director-ring to be present")
	}
	if certHasSAN(path, "rel-director-nope") {
		t.Error("absent SAN must return false")
	}
	// Unreadable cert is best-effort true (no spurious warning).
	if !certHasSAN(filepath.Join(t.TempDir(), "missing.pem"), "x") {
		t.Error("missing cert must be treated as best-effort true")
	}
}
