package imap

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

	"github.com/0kaba0hub/yarilo/pkg/mtls"
)

// writeInternalCerts generates a self-signed internal CA and one shared leaf
// (SAN "yarilo-internal", both server+client ExtKeyUsage) — exactly the sandbox
// internal-tls setup — and returns the cert/key/ca file paths.
func writeInternalCerts(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "yarilo-internal-ca"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<31, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caFile = filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caCert, _ := x509.ParseCertificate(caDER)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "yarilo-internal"},
		DNSNames:     []string{"yarilo-internal"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	certFile = filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(leafKey)
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, caFile
}

// TestWrapProxy_InternalMTLSHandshake guards #826: with the real
// mtls.ServerConfig (RequireAndVerifyClientCert) as PreambleTLS AND
// MaxLineLength set, a client's internal-mTLS handshake through the imap
// listener chain must COMPLETE. The regression was the maxLineLen wrapper
// buffering + line-scanning the binary ClientHello beneath TLS, so the server
// never sent a ServerHello. This exercises the real config pair the #824 unit
// test (simplified server cfg, no maxLineLen) missed.
func TestWrapProxy_InternalMTLSHandshake(t *testing.T) {
	certFile, keyFile, caFile := writeInternalCerts(t)
	serverCfg, err := mtls.ServerConfig(certFile, keyFile, caFile)
	if err != nil {
		t.Fatal(err)
	}
	clientCfg, err := mtls.ClientConfig(certFile, keyFile, caFile, "yarilo-internal", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rawLn.Close() })

	// The chain that broke #826: MaxLineLength active + internal-mTLS preamble.
	// AuthAddr points nowhere — the TLS handshake must still complete (the
	// preamble/auth step fails afterwards, which is fine for this assertion).
	s := &Server{opts: Options{MaxLineLength: 512, AuthAddr: "127.0.0.1:1", PreambleTLS: serverCfg}}
	wrapped := s.wrapProxy(rawLn)
	go func() { _, _ = wrapped.Accept() }() // drive the server-side handshake

	conn, err := net.Dial("tcp", rawLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	tc := tls.Client(conn, clientCfg)
	if err := tc.Handshake(); err != nil {
		t.Fatalf("internal mTLS handshake through the imap listener chain must complete, got: %v", err)
	}
}
