package jmap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func testLimits() jmapcore.Limits {
	return jmapcore.Limits{
		BaseURL:               "https://mail.example.com",
		MaxSizeUpload:         41943040,
		MaxSizeRequest:        10485760,
		MaxConcurrentRequests: 10,
		MaxCallsInRequest:     16,
		MaxObjectsInGet:       500,
		MaxObjectsInSet:       500,
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("cidr %s: %v", s, err)
	}
	return n
}

// identityRequest is what the login layer sends, minus the transport.
func identityRequest(user, peer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, jmapcore.SessionPath, nil)
	r.RemoteAddr = peer
	r.Header.Set(hdrUser, user)
	r.Header.Set(hdrSessionID, "deadbeefdeadbeef")
	r.Header.Set(hdrProxyTTL, "4")
	r.Header.Set(hdrForwarded, `for="203.0.113.7:51234";proto=https;by="10.1.2.3"`)
	return r
}

// Mode 3: with no anchor configured the backend still answers, and answers 403.
// A dead port would read as a network fault; this diagnoses itself.
func TestTrustNoneRefusesEveryIdentityRequest(t *testing.T) {
	s := New(Options{Trust: ResolveTrust(false, false, nil), Limits: testLimits()})
	tests := []struct {
		name, peer string
	}{
		{"loopback", "127.0.0.1:40000"},
		{"pod network", "10.42.0.9:40000"},
		{"public", "203.0.113.7:40000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, identityRequest("u1@example.com", tt.peer))
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
		})
	}
}

// Mode 2: the anchor is the address, so a peer outside the CIDRs is refused
// however well-formed its headers are.
func TestTrustNetsHonoursOnlyConfiguredCIDRs(t *testing.T) {
	nets := []*net.IPNet{mustCIDR(t, "10.42.0.0/16")}
	s := New(Options{Trust: ResolveTrust(false, true, nets), Limits: testLimits()})
	tests := []struct {
		name, peer string
		want       int
	}{
		{"inside", "10.42.0.9:40000", http.StatusOK},
		{"outside", "10.43.0.9:40000", http.StatusForbidden},
		{"loopback", "127.0.0.1:40000", http.StatusForbidden},
		{"public", "203.0.113.7:40000", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, identityRequest("u1@example.com", tt.peer))
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// An empty trusted_nets list must not degrade to trusting everyone.
func TestTrustNetsEmptyListIsNotTrustEveryone(t *testing.T) {
	if got := ResolveTrust(false, true, nil).Mode; got != TrustNone {
		t.Errorf("mode = %v, want none", got)
	}
}

// mTLS wins over an address list, or the weaker anchor would decide.
func TestTrustMTLSTakesPrecedence(t *testing.T) {
	nets := []*net.IPNet{mustCIDR(t, "10.42.0.0/16")}
	if got := ResolveTrust(true, true, nets).Mode; got != TrustMTLS {
		t.Errorf("mode = %v, want mtls", got)
	}
}

// Mode 1: a certificate from any other authority is refused. This runs over a
// real handshake, since that is where the rejection happens.
func TestTrustMTLSRejectsForeignCertificate(t *testing.T) {
	ours := newTestCA(t, "yarilo-internal")
	theirs := newTestCA(t, "somebody-else")
	srvCert, srvKey := ours.issue(t, "yarilo-jmap")

	srvTLS, err := mtls.ServerConfig(srvCert, srvKey, ours.caFile())
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	addr := serveOn(t, Options{
		TLSConfig: srvTLS,
		Trust:     ResolveTrust(true, false, nil),
		Limits:    testLimits(),
	})

	tests := []struct {
		name   string
		ca     *testCA
		wantOK bool
	}{
		{name: "our CA", ca: ours, wantOK: true},
		{name: "foreign CA", ca: theirs, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cliCert, cliKey := tt.ca.issue(t, "yarilo-jmap-login")
			pair, err := tls.LoadX509KeyPair(cliCert, cliKey)
			if err != nil {
				t.Fatalf("client pair: %v", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(ours.pemDER) {
				t.Fatal("client trust pool")
			}
			c := &http.Client{
				Timeout: 5 * time.Second,
				Transport: &http.Transport{TLSClientConfig: &tls.Config{
					Certificates: []tls.Certificate{pair},
					RootCAs:      pool,
					ServerName:   "yarilo-jmap",
					MinVersion:   tls.VersionTLS13,
				}},
			}
			req, err := http.NewRequestWithContext(context.Background(),
				http.MethodGet, "https://"+addr+jmapcore.SessionPath, nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set(hdrUser, "u1@example.com")
			req.Header.Set(hdrProxyTTL, "4")
			resp, err := c.Do(req)
			if !tt.wantOK {
				if err == nil {
					resp.Body.Close() //nolint:errcheck
					t.Fatalf("a foreign certificate was accepted: status %d", resp.StatusCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

// serveOn starts the server on an ephemeral port and returns its address.
func serveOn(t *testing.T, opts Options) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}
	opts.Addr = addr
	s := New(opts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close() //nolint:errcheck
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never accepted", addr)
	return ""
}
