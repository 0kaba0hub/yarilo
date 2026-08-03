package jmap

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// serveOn starts the server on an ephemeral port and returns its address.
func serveOn(t *testing.T, opts Options) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}
	opts.Addr = addr
	if opts.Protocol.BaseURL == "" {
		opts.Protocol = testProtocol()
	}
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
	waitReady(t, addr)
	return addr
}

func waitReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close() //nolint:errcheck
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never accepted", addr)
}

// Serve must bind, answer and then shut down on context cancel; the cleanup in
// serveOn fails the test if it hangs.
func TestServeAnswersAndShutsDown(t *testing.T) {
	addr := serveOn(t, Options{Auth: &fakeAuth{user: "u1", pass: "pw"}})

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+sessionPath, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", basic("u1", "pw"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// The listener stops accepting past the cap, so a burst cannot exhaust the
// process. The limiter queues rather than rejecting, so the check is that a
// connection beyond the cap does not complete a request while the first is held.
func TestConnectionLimitCapsConcurrency(t *testing.T) {
	addr := serveOn(t, Options{
		Auth:            &fakeAuth{user: "u1", pass: "pw"},
		ConnectionLimit: 1,
	})

	held, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer held.Close() //nolint:errcheck

	// The cap is taken, so a second request must not be answered while it is.
	client := &http.Client{Timeout: 400 * time.Millisecond}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+sessionPath, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", basic("u1", "pw"))
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close() //nolint:errcheck
		t.Fatal("a request beyond the connection limit was served")
	}

	// Releasing the held connection lets the next one through, so the limiter
	// gates rather than breaks.
	held.Close() //nolint:errcheck
	client.Timeout = 3 * time.Second
	req2, err := http.NewRequest(http.MethodGet, "http://"+addr+sessionPath, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req2.Header.Set("Authorization", basic("u1", "pw"))
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("request after release: %v", err)
	}
	defer resp2.Body.Close() //nolint:errcheck
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status after release = %d, want 200", resp2.StatusCode)
	}
}

// A PROXY header from an untrusted peer must be ignored rather than trusted:
// a client that picks its own source address defeats allow_nets.
func TestProxyPolicyTrustsOnlyConfiguredNets(t *testing.T) {
	_, trusted, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	policy := proxyPolicy([]*net.IPNet{trusted})

	inside, err := policy(&net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 1})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if inside != proxyproto.USE {
		t.Errorf("trusted peer policy = %v, want USE", inside)
	}
	outside, err := policy(&net.TCPAddr{IP: net.ParseIP("203.0.113.1"), Port: 1})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if outside == proxyproto.USE {
		t.Error("an untrusted peer's PROXY header would be trusted")
	}

	// No configured nets means no header is trusted at all.
	empty := proxyPolicy(nil)
	got, err := empty(&net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 1})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if got == proxyproto.USE {
		t.Error("an empty trusted list trusted a PROXY header")
	}
}
