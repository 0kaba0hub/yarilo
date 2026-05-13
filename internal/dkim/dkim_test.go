package dkim

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generateRSAKey returns a 2048-bit RSA private key.
func generateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

// writePEM writes a PKCS1 RSA private key PEM to a temp file and returns the path.
func writePEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	dir := t.TempDir()
	p := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatalf("write pem: %v", err)
	}
	return p
}

const testMsg = "From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: test\r\n\r\nHello\r\n"

func TestSignAndVerify(t *testing.T) {
	key := generateRSAKey(t)

	cfg := SignConfig{
		Selector:        "sel",
		SignHeaders:     []string{"From", "To", "Subject"},
		OversignHeaders: []string{"From"},
	}

	var signed bytes.Buffer
	if err := Sign(&signed, strings.NewReader(testMsg), "example.com", key, cfg); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// The signed message must contain a DKIM-Signature header.
	if !strings.Contains(signed.String(), "DKIM-Signature:") {
		t.Fatal("signed message missing DKIM-Signature header")
	}
}

func TestVerify_NoSignature(t *testing.T) {
	results, err := Verify(strings.NewReader(testMsg))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for unsigned message, got %d", len(results))
	}
}

var normaliseQueryCases = []struct {
	q      string
	driver string
	want   string
}{
	{"SELECT key FROM t WHERE domain = ?", "sqlite", "SELECT key FROM t WHERE domain = ?"},
	{"SELECT key FROM t WHERE domain = ?", "postgres", "SELECT key FROM t WHERE domain = $1"},
	{"SELECT key FROM t WHERE domain = $1", "postgres", "SELECT key FROM t WHERE domain = $1"},
	{"SELECT key FROM t WHERE domain = $1", "mysql", "SELECT key FROM t WHERE domain = ?"},
}

func TestNormaliseQuery(t *testing.T) {
	for _, tc := range normaliseQueryCases {
		got := normaliseQuery(tc.q, tc.driver)
		if got != tc.want {
			t.Errorf("normaliseQuery(%q, %q) = %q, want %q", tc.q, tc.driver, got, tc.want)
		}
	}
}

func TestStaticKeyProvider_HappyPath(t *testing.T) {
	key := generateRSAKey(t)
	pemPath := writePEM(t, key)

	p := NewStaticKeyProvider(map[string]string{"example.com": pemPath})
	k, err := p.GetPrivateKey(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("GetPrivateKey: %v", err)
	}
	if k == nil {
		t.Fatal("expected non-nil signer")
	}

	// Second call hits the cache.
	k2, err := p.GetPrivateKey(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("GetPrivateKey (cached): %v", err)
	}
	if k != k2 {
		t.Fatal("expected same signer from cache")
	}
}

func TestStaticKeyProvider_UnknownDomain(t *testing.T) {
	p := NewStaticKeyProvider(map[string]string{})
	_, err := p.GetPrivateKey(context.Background(), "unknown.com")
	if err == nil {
		t.Fatal("expected error for unknown domain")
	}
}

func TestStaticKeyProvider_MissingFile(t *testing.T) {
	p := NewStaticKeyProvider(map[string]string{"x.com": "/nonexistent/key.pem"})
	_, err := p.GetPrivateKey(context.Background(), "x.com")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParsePEMKey_InvalidType(t *testing.T) {
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")})
	_, err := parsePEMKey(data)
	if err == nil {
		t.Fatal("expected error for unsupported PEM type")
	}
}

func TestParsePEMKey_NoPEMBlock(t *testing.T) {
	_, err := parsePEMKey([]byte("not a pem"))
	if err == nil {
		t.Fatal("expected error when no PEM block present")
	}
}
