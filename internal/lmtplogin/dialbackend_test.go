package lmtplogin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// selfSignedTLS builds a server tls.Config with a fresh self-signed cert for
// 127.0.0.1 and a client tls.Config that trusts it.
func selfSignedTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "yarilo-backend-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	serverCfg := &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}}}
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}
	return serverCfg, clientCfg
}

// TestDialBackend_TLSHandshake guards #739: a non-nil tlsCfg makes the backend
// fan-out dial complete a TLS handshake; nil dials plain TCP.
func TestDialBackend_TLSHandshake(t *testing.T) {
	serverCfg, clientCfg := selfSignedTLS(t)

	t.Run("tls dial handshakes against a TLS backend", func(t *testing.T) {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			c, aerr := ln.Accept()
			if aerr == nil {
				_ = c.(*tls.Conn).Handshake()
				c.Close()
			}
		}()

		conn, err := dialBackend(ln.Addr().String(), 2*time.Second, clientCfg)
		if err != nil {
			t.Fatalf("tls dialBackend: %v", err)
		}
		defer conn.Close()
		tc, ok := conn.(*tls.Conn)
		if !ok {
			t.Fatalf("want *tls.Conn, got %T", conn)
		}
		if err := tc.Handshake(); err != nil {
			t.Fatalf("client handshake: %v", err)
		}
	})

	t.Run("nil tlsCfg dials plain TCP", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			if c, aerr := ln.Accept(); aerr == nil {
				c.Close()
			}
		}()

		conn, err := dialBackend(ln.Addr().String(), 2*time.Second, nil)
		if err != nil {
			t.Fatalf("plain dialBackend: %v", err)
		}
		defer conn.Close()
		if _, ok := conn.(*tls.Conn); ok {
			t.Fatal("nil tlsCfg must not return a *tls.Conn")
		}
	})

	t.Run("tls dial against a plain backend fails", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			if c, aerr := ln.Accept(); aerr == nil {
				// Send noise so the client TLS handshake fails rather than hangs.
				_, _ = c.Write([]byte("220 not-tls\r\n"))
				time.Sleep(50 * time.Millisecond)
				c.Close()
			}
		}()

		conn, err := dialBackend(ln.Addr().String(), 2*time.Second, clientCfg)
		if err == nil {
			conn.Close()
			t.Fatal("tls dial against a plain backend should fail the handshake")
		}
	})
}
