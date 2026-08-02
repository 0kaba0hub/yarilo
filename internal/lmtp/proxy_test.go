package lmtp

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// buildBackendServer starts a real LMTP backend server and returns its address,
// mailbox backend, and resolver for message count verification.
func buildBackendServer(t *testing.T, users ...string) (addr string, mb mailbox.MailboxBackend, resolver *mailbox.Resolver) {
	t.Helper()
	dir := t.TempDir()
	resolver = &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mbox := maildir.New()
	idx := fileindex.New()

	for _, u := range users {
		box := mbox.OpenUser(resolver.UserInfo(u, ""))
		if err := box.Init(); err != nil {
			t.Fatalf("Init %s: %v", u, err)
		}
		box.Close() //nolint:errcheck
	}

	srv := New(Options{
		Hostname: "backend.test",
		Config: config.LMTPProtocolConfig{
			AddReceivedHeader: false,
			ReadTimeout:       5,
			WriteTimeout:      5,
		},
		Mailbox:  mbox,
		Index:    idx,
		Resolver: resolver,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), mbox, resolver
}

// ringRouter adapts a *ring.Ring to the UserRouter interface for testing.
type ringRouter struct {
	r *ring.Ring
}

func (rr *ringRouter) RouteUser(username string) (string, error) {
	b := rr.r.LookupBackend(username)
	if b == nil {
		return "", fmt.Errorf("no backend available")
	}
	return b.IP, nil
}

// buildProxyServer starts an LMTP proxy server using a ring built from the
// given backend addresses.
func buildProxyServer(t *testing.T, backends ...string) string {
	t.Helper()
	r := ring.New(ring.DefaultHashFormat())
	backendPort := 24
	for _, b := range backends {
		host, portStr, _ := net.SplitHostPort(b)
		port := 24
		if portStr != "" {
			fmt.Sscanf(portStr, "%d", &port)
		}
		backendPort = port
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
		Router:      &ringRouter{r: r},
		BackendPort: backendPort,
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
	backendAddr, mb, resolver := buildBackendServer(t, "alice@example.com")
	proxyAddr := buildProxyServer(t, backendAddr)

	conn, sc := dialLMTP(t, proxyAddr)
	sendLHLO(t, conn, sc)

	resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
	if !strings.HasPrefix(resp[0], "250") {
		t.Fatalf("expected 250 via proxy, got: %q", resp[0])
	}

	box := mb.OpenUser(resolver.UserInfo("alice@example.com", ""))
	msgs, err := box.List("INBOX")
	box.Close() //nolint:errcheck
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
		Router: &ringRouter{r: ring.New(ring.DefaultHashFormat())}, // empty ring
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
	r := ring.New(ring.DefaultHashFormat())
	r.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 24, Up: true})
	r.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 24, Up: true})

	router := newProxyRouter("test", &ringRouter{r: r}, 24, 5*time.Second)

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
