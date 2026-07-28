package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
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
	_, err := ClientConfig("nonexistent.crt", "nonexistent.key", "nonexistent.ca", "", 0, 0)
	if err == nil {
		t.Fatal("empty ServerName must fail")
	}
	if !strings.Contains(err.Error(), "server_name") {
		t.Fatalf("error must hint at internal_tls.server_name, got: %v", err)
	}
}

func TestClientConfig_PinsServerName(t *testing.T) {
	certFile, keyFile := writeSelfSigned(t)
	cfg, err := ClientConfig(certFile, keyFile, certFile, "yarilo-internal", 0, 0)
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

func writeSelfSigned(t testing.TB) (certFile, keyFile string) {
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

// TestClientConfig_SessionResumption proves the #856 fix end-to-end: a second
// dial to the same mTLS server resumes via a cached TLS 1.3 session ticket
// instead of a full handshake. Loopback only — no external service.
func TestClientConfig_SessionResumption(t *testing.T) {
	certFile, keyFile := writeSelfSigned(t)
	srvCfg, err := ServerConfig(certFile, keyFile, certFile)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	cliCfg, err := ClientConfig(certFile, keyFile, certFile, "yarilo-internal", 0, 0)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if cliCfg.ClientSessionCache == nil {
		t.Fatal("ClientConfig must set a ClientSessionCache for resumption (#856)")
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				_ = c.(*tls.Conn).Handshake()
				_, _ = c.Write([]byte("ok")) // flush the post-handshake session ticket
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()

	dialResumed := func() bool {
		c, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close() //nolint:errcheck
		if err := c.Handshake(); err != nil {
			t.Fatalf("handshake: %v", err)
		}
		// Reading the server's first bytes makes the client process the
		// NewSessionTicket message and cache it.
		buf := make([]byte, 2)
		_, _ = io.ReadFull(c, buf)
		return c.ConnectionState().DidResume
	}

	if dialResumed() {
		t.Fatal("first dial must be a full handshake, not resumed")
	}
	if !dialResumed() {
		t.Fatal("second dial must resume via the cached session ticket — ClientSessionCache not effective")
	}
}

// BenchmarkMTLSHandshake quantifies the #856 tax: a full mTLS handshake versus a
// resumed one, against a loopback server. The delta is the per-dial CPU the
// ClientSessionCache saves on repeated internal dials. (Absolute numbers are
// machine-specific; the ratio is the point.)
func BenchmarkMTLSHandshake(b *testing.B) {
	certFile, keyFile := writeSelfSigned(b)
	srvCfg, err := ServerConfig(certFile, keyFile, certFile)
	if err != nil {
		b.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				_ = c.(*tls.Conn).Handshake()
				_, _ = c.Write([]byte("ok"))
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()

	dial := func(cfg *tls.Config) bool {
		c, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close() //nolint:errcheck
		buf := make([]byte, 2)
		_, _ = io.ReadFull(c, buf)
		return c.ConnectionState().DidResume
	}

	b.Run("full", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			// Fresh cache each iteration → always a full handshake.
			cfg, _ := ClientConfig(certFile, keyFile, certFile, "yarilo-internal", 0, 0)
			dial(cfg)
		}
	})
	b.Run("resumed", func(b *testing.B) {
		cfg, _ := ClientConfig(certFile, keyFile, certFile, "yarilo-internal", 0, 0)
		dial(cfg) // warm the cache (full handshake, not counted below)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if !dial(cfg) {
				b.Fatal("expected resumed handshake")
			}
		}
	})
}

// TestNewSessionCache locks the size/TTL policy: negative disables resumption,
// zero uses the default, and a positive TTL wraps the LRU (#856).
func TestNewSessionCache(t *testing.T) {
	if newSessionCache(-1, 0) != nil {
		t.Fatal("negative size must disable resumption (nil cache)")
	}
	if newSessionCache(0, 0) == nil {
		t.Fatal("zero size must fall back to the default cache")
	}
	if _, ok := newSessionCache(8, 60).(*ttlSessionCache); !ok {
		t.Fatal("ttl>0 must wrap the LRU in a ttlSessionCache")
	}
	if _, ok := newSessionCache(8, 0).(*ttlSessionCache); ok {
		t.Fatal("ttl<=0 must be a plain LRU, not a ttlSessionCache")
	}
}

// TestTTLSessionCache_Expires verifies a cached session past its TTL is treated
// as absent (no sleep — the put timestamp is aged directly).
func TestTTLSessionCache_Expires(t *testing.T) {
	c := &ttlSessionCache{lru: tls.NewLRUClientSessionCache(4), ttl: time.Minute, put: map[string]time.Time{}}
	cs := &tls.ClientSessionState{}
	c.Put("k", cs)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("fresh session must be resumable")
	}
	c.put["k"] = time.Now().Add(-2 * time.Minute) // age past the TTL
	if _, ok := c.Get("k"); ok {
		t.Fatal("session past its TTL must not resume")
	}
	if _, tracked := c.put["k"]; tracked {
		t.Fatal("expired entry must be dropped from the tracking map")
	}
}
