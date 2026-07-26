package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClientConfig_EmptyServerNameFails guards the #816 loud fail: an empty
// ServerName (typically an empty internal_tls.server_name) errors immediately
// with a fix hint, before any cert I/O.
func TestClientConfig_EmptyServerNameFails(t *testing.T) {
	_, err := ClientConfig("nonexistent.crt", "nonexistent.key", "nonexistent.ca", "")
	if err == nil {
		t.Fatal("empty ServerName must fail")
	}
	if !strings.Contains(err.Error(), "server_name") {
		t.Fatalf("error must hint at internal_tls.server_name, got: %v", err)
	}
}

func TestClientConfig_PinsServerName(t *testing.T) {
	certFile, keyFile := writeSelfSigned(t)
	cfg, err := ClientConfig(certFile, keyFile, certFile, "yarilo-internal")
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if cfg.ServerName != "yarilo-internal" {
		t.Fatalf("ServerName = %q, want yarilo-internal", cfg.ServerName)
	}
	if cfg.RootCAs == nil {
		t.Fatal("RootCAs must be set (client verification), not a server config")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatal("client cert must be presented")
	}
}

func writeSelfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
		DNSNames:     []string{"yarilo-internal"},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
