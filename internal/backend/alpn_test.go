package backend

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/config"
)

// writeTestCert generates a self-signed EC cert and writes cert.pem/key.pem
// to a temp dir, returning the dir path.
func writeTestCert(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "yarilo-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBuildTLS_ALPNSet(t *testing.T) {
	dir := writeTestCert(t)
	cfg := &config.Config{
		General: config.GeneralConfig{SSL: config.SSLConfig{
			TLSCert: filepath.Join(dir, "cert.pem"),
			TLSKey:  filepath.Join(dir, "key.pem"),
		}},
	}
	svc := &config.ServiceConfig{Enabled: true, Port: 993, SSLMode: "ssl"}

	cases := []struct {
		name  string
		alpn  []string
		wants []string
	}{
		{"imap", []string{alpnIMAP}, []string{"imap"}},
		{"pop3", []string{alpnPOP3}, []string{"pop3"}},
		{"smtp", []string{alpnSMTP}, []string{"smtp"}},
		{"none", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildTLS(cfg, svc, tc.alpn...)
			if err != nil {
				t.Fatalf("buildTLS: %v", err)
			}
			if !equalStrings(got.NextProtos, tc.wants) {
				t.Errorf("NextProtos = %v, want %v", got.NextProtos, tc.wants)
			}
		})
	}
}

func TestBuildTLS_ALPNMismatchRejected(t *testing.T) {
	dir := writeTestCert(t)
	cfg := &config.Config{
		General: config.GeneralConfig{SSL: config.SSLConfig{
			TLSCert: filepath.Join(dir, "cert.pem"),
			TLSKey:  filepath.Join(dir, "key.pem"),
		}},
	}
	svc := &config.ServiceConfig{Enabled: true, Port: 993, SSLMode: "ssl"}

	srvCfg, err := buildTLS(cfg, svc, alpnIMAP)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				_ = conn.(*tls.Conn).HandshakeContext(t.Context())
				conn.Close()
			}(c)
		}
	}()

	// Wrong ALPN should fail
	clientCfg := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"pop3"}} //nolint:gosec
	if _, err := tls.Dial("tcp", ln.Addr().String(), clientCfg); err == nil {
		t.Fatal("expected handshake failure with mismatched ALPN, got nil")
	}

	// Matching ALPN should succeed
	clientCfg.NextProtos = []string{"imap"}
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("expected handshake to succeed with imap ALPN, got %v", err)
	}
	if conn.ConnectionState().NegotiatedProtocol != "imap" {
		t.Errorf("NegotiatedProtocol = %q, want imap", conn.ConnectionState().NegotiatedProtocol)
	}
	conn.Close() //nolint:errcheck

	// No ALPN at all should succeed (missing ALPN is accepted)
	clientCfg.NextProtos = nil
	conn, err = tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("expected handshake to succeed without ALPN, got %v", err)
	}
	conn.Close() //nolint:errcheck
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
