package main

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The row could not run against the reference deployment at all: the admin API
// is served with mutual TLS and the row authenticated with a bearer token only,
// so it never got past the handshake (#1280). Both directions are asserted
// against a real mTLS server: with the certificate flags the row reads numbers,
// without them it fails at the handshake rather than reporting a quota.
func TestQuotaRowReachesAnMTLSAdminAPI(t *testing.T) {
	dir := t.TempDir()
	caPEM, serverCert, clientCertPEM, clientKeyPEM := issueMTLSFixtures(t)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA not usable")
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":"u@x.com","storage_value":2048,"storage_limit":10240}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	defer srv.Close()

	certPath := writeFile(t, dir, "client.crt", clientCertPEM)
	keyPath := writeFile(t, dir, "client.key", clientKeyPEM)
	caPath := writeFile(t, dir, "ca.crt", caPEM)

	restore := setFlags(map[*string]string{
		flagBackendAPI:     srv.URL,
		flagBackendAPICert: certPath,
		flagBackendAPIKey:  keyPath,
		flagBackendAPICA:   caPath,
	})
	defer restore()

	got, err := adminReadQuota("u@x.com")
	if err != nil {
		t.Fatalf("with client certificate: %v", err)
	}
	if got.fields["storageUsedKiB"] != "2048" || got.fields["storageLimitKiB"] != "10240" {
		t.Errorf("quota reading = %v", got.fields)
	}

	// Without the certificate the server refuses the handshake, and the row
	// must surface that rather than report a quota it never read.
	*flagBackendAPICert, *flagBackendAPIKey = "", ""
	if _, err := adminReadQuota("u@x.com"); err == nil {
		t.Error("an mTLS endpoint answered without a client certificate")
	}
}

// Half a credential is a configuration error, not a fallback to anonymous: a
// row that silently dropped the certificate would fail at the handshake with a
// message about the server instead of about the flags.
func TestQuotaClientRefusesHalfACredential(t *testing.T) {
	restore := setFlags(map[*string]string{
		flagBackendAPI:     "https://yarilo-backend:9105",
		flagBackendAPICert: "/tmp/absent.crt",
		flagBackendAPIKey:  "",
	})
	defer restore()

	_, err := backendAPIClient()
	if err == nil {
		t.Fatal("a certificate without its key was accepted")
	}
	if !strings.Contains(err.Error(), "-backend-api-key") {
		t.Errorf("error %q does not name the missing flag", err)
	}
}

// The skip an operator reads has to name the mTLS requirement: with only
// -backend-api against the reference deployment the row fails at the
// handshake, and the skip is the first place that could have said so.
func TestAdminAPISkipNamesTheCertificateFlags(t *testing.T) {
	var checks []check
	registerConsistency(&checks)

	var found bool
	for _, c := range checks {
		if !strings.Contains(c.name, "quota") || !strings.Contains(c.name, "admin API") {
			continue
		}
		found = true
		if c.skip == "" {
			t.Fatalf("row %q is runnable in a test process with no -backend-api", c.name)
		}
		for _, want := range []string{"-backend-api", "-backend-api-cert", "mTLS"} {
			if !strings.Contains(c.skip, want) {
				t.Errorf("skip %q does not name %q", c.skip, want)
			}
		}
	}
	if !found {
		t.Fatal("no admin API quota row registered")
	}
}

func setFlags(values map[*string]string) func() {
	old := make(map[*string]string, len(values))
	for p, v := range values {
		old[p] = *p
		*p = v
	}
	return func() {
		for p, v := range old {
			*p = v
		}
	}
}

func writeFile(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// issueMTLSFixtures mints a CA, a server certificate for 127.0.0.1 and a client
// certificate, all PEM. Self-contained so the row's transport is exercised
// against a server that really demands a client certificate.
func issueMTLSFixtures(t *testing.T) (caPEM []byte, serverCert tls.Certificate, clientCertPEM, clientKeyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "smoke test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leaf := func(cn string, server bool) (tls.Certificate, []byte, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if server {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.DNSNames = []string{"127.0.0.1", "localhost"}
			tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP("127.0.0.1"))
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return pair, certPEM, keyPEM
	}

	serverCert, _, _ = leaf("127.0.0.1", true)
	_, clientCertPEM, clientKeyPEM = leaf("yarilo-smoketest", false)
	return caPEM, serverCert, clientCertPEM, clientKeyPEM
}
