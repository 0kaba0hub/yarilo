package loginproto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func testServerTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "yarilo-internal"},
		DNSNames:     []string{"yarilo-internal"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
}

func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-ch
	t.Cleanup(func() { server.Close(); client.Close() })
	return server, client
}

// TestPreambleListener_TerminatesTLS guards #824: with TLSConfig set, the
// PreambleListener terminates internal mTLS BEFORE reading the preamble — so a
// plaintext preamble is rejected at the TLS handshake (not misread as "not a
// YARILO preamble"), and a preamble sent over TLS is read correctly (the
// handshake then fails only later, at the unreachable auth dial).
func TestPreambleListener_TerminatesTLS(t *testing.T) {
	l := &PreambleListener{AuthAddr: "127.0.0.1:1", TLSConfig: testServerTLS(t)}

	// (a) plaintext to a TLS-terminating listener → rejected at TLS handshake.
	srv, cli := tcpPair(t)
	go func() { _, _ = cli.Write([]byte("YARILO\tADDR=1.2.3.4\tUSER=u\tTOKEN=t\n")) }()
	_, err := l.handshake(srv)
	if err == nil || !strings.Contains(err.Error(), "mtls handshake") {
		t.Fatalf("plaintext must fail at the TLS handshake, got %v", err)
	}

	// (b) preamble over TLS → TLS terminates, preamble is read; the only failure
	// is the (unreachable) auth dial, proving the preamble parsed cleanly.
	srv2, cli2 := tcpPair(t)
	go func() {
		tc := tls.Client(cli2, &tls.Config{InsecureSkipVerify: true})
		if err := tc.Handshake(); err != nil {
			return
		}
		_, _ = tc.Write([]byte("YARILO\tADDR=1.2.3.4\tUSER=u\tTOKEN=t\n"))
	}()
	_, err = l.handshake(srv2)
	if err == nil {
		t.Fatal("expected an error (auth unreachable)")
	}
	if strings.Contains(err.Error(), "mtls handshake") || strings.Contains(err.Error(), "not a YARILO") {
		t.Fatalf("over TLS the preamble must be read cleanly; expected an auth-dial failure, got %v", err)
	}
}
