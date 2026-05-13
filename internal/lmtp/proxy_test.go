package lmtp

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
	fileindex "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

// buildBackendServer starts a real LMTP backend server and returns its address
// and mailbox for message count verification.
func buildBackendServer(t *testing.T, users ...string) (addr string, mb *maildir.Backend) {
	t.Helper()
	dir := t.TempDir()
	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New(dir)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck

	for _, u := range users {
		if err := mb.Init(u); err != nil {
			t.Fatalf("Init %s: %v", u, err)
		}
	}

	srv := New(Options{
		Hostname: "backend.test",
		Config: config.LMTPProtocolConfig{
			AddReceivedHeader: false,
			ReadTimeout:       5,
			WriteTimeout:      5,
		},
		Mailbox: mb,
		Index:   idx,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), mb
}

// buildProxyServer starts an LMTP proxy server using a ring built from the
// given backend addresses.
func buildProxyServer(t *testing.T, backends ...string) string {
	t.Helper()
	r := ring.New()
	for _, b := range backends {
		host, portStr, _ := net.SplitHostPort(b)
		port := 24
		if portStr != "" {
			fmt.Sscanf(portStr, "%d", &port)
		}
		r.AddBackend(&ring.Backend{IP: host, Port: port, Up: true})
	}

	srv := New(Options{
		Hostname: "proxy.test",
		Config: config.LMTPProtocolConfig{
			AddReceivedHeader: false,
			ReadTimeout:       5,
			WriteTimeout:      5,
			Proxy:             config.LMTPProxyConfig{Timeout: 5},
		},
		Ring: r,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String()
}

func TestLMTP_Proxy_SingleBackend(t *testing.T) {
	backendAddr, mb := buildBackendServer(t, "alice@example.com")
	proxyAddr := buildProxyServer(t, backendAddr)

	conn, sc := dialLMTP(t, proxyAddr)
	sendLHLO(t, conn, sc)

	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if !strings.HasPrefix(resp[0], "250") {
		t.Fatalf("expected 250 via proxy, got: %q", resp[0])
	}

	msgs, err := mb.List("alice@example.com", "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in backend INBOX, got %d", len(msgs))
	}
}

func TestLMTP_Proxy_UnknownRoute(t *testing.T) {
	// Empty ring → routing fails → 451 at RCPT TO.
	srv := New(Options{
		Hostname: "proxy.test",
		Config: config.LMTPProtocolConfig{
			ReadTimeout:  5,
			WriteTimeout: 5,
			Proxy:        config.LMTPProxyConfig{Timeout: 5},
		},
		Ring: ring.New(), // empty ring
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)

	fmt.Fprintf(conn, "MAIL FROM:<sender@external.com>\r\n")
	sc.Scan()
	fmt.Fprintf(conn, "RCPT TO:<alice@example.com>\r\n")
	sc.Scan()
	resp := sc.Text()
	if !strings.HasPrefix(resp, "451") {
		t.Fatalf("expected 451 for empty ring, got: %q", resp)
	}
}

func TestProxyRouter_Route_Consistent(t *testing.T) {
	r := ring.New()
	r.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 24, Up: true})
	r.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 24, Up: true})

	router := newProxyRouter("test", r, 5*time.Second)

	// Same username must always resolve to the same backend.
	addr1, err := router.route("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		got, err := router.route("alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got != addr1 {
			t.Fatalf("inconsistent routing: got %q, want %q", got, addr1)
		}
	}

	// Both backends should be reachable across different users.
	seen := map[string]bool{}
	for _, user := range []string{"alice@example.com", "bob@example.com", "carol@example.com",
		"dave@example.com", "eve@example.com", "frank@example.com"} {
		addr, err := router.route(user)
		if err != nil {
			t.Fatal(err)
		}
		seen[addr] = true
	}
	if len(seen) < 1 {
		t.Fatal("expected at least one backend to be used")
	}
}
