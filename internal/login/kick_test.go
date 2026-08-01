package login

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/anvil"
)

// cuttableProxy is a TCP passthrough whose live connections can be severed on
// demand, so a test can simulate an anvil restart / network blip between a login
// pod and anvil WITHOUT restarting the server.
type cuttableProxy struct {
	ln      net.Listener
	backend string
	mu      sync.Mutex
	conns   []net.Conn
}

func newCuttableProxy(t *testing.T, backend string) *cuttableProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &cuttableProxy{ln: ln, backend: backend}
	go p.accept()
	t.Cleanup(func() { ln.Close(); p.cut() })
	return p
}

func (p *cuttableProxy) addr() string { return p.ln.Addr().String() }

func (p *cuttableProxy) accept() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.backend)
		if err != nil {
			down.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, down, up)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, down) }()
		go func() { _, _ = io.Copy(down, up) }()
	}
}

// cut severs every live connection, simulating the blip.
func (p *cuttableProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// startEmbeddedAnvil spins an in-process anvil server on a
// random TCP port and returns its address. The server stops
// when the test finishes.
func startEmbeddedAnvil(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := anvil.NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.ListenAndServe(ctx, addr, nil)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return addr
}

// TestKickSession_ClosesBackendConn verifies the core invariant:
// after kickSession runs against a live entry, the backendConn
// of that session is closed.
func TestKickSession_ClosesBackendConn(t *testing.T) {
	s := &Server{sessions: make(map[string][]*liveSession)}
	cliEnd, srvEnd := net.Pipe()
	defer cliEnd.Close()
	sess := &liveSession{id: "abc123", user: "alice@example.com", backendConn: srvEnd}
	s.sessions["alice@example.com"] = []*liveSession{sess}

	if !s.kickSession("abc123") {
		t.Fatal("kickSession returned false for live id")
	}
	cliEnd.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := cliEnd.Read(buf); err == nil {
		t.Error("backendConn read returned no error after kick — conn still open")
	}
}

func TestKickSession_NoMatchIsNoop(t *testing.T) {
	s := &Server{sessions: make(map[string][]*liveSession)}
	if s.kickSession("ghost") {
		t.Fatal("kickSession returned true for missing id")
	}
}

// TestKickSubscriberDispatchesEvent boots an in-process anvil
// server, starts the login subscriber, EMITs a kick event from
// a separate anvil conn and asserts the session is closed
// end-to-end.
func TestKickSubscriberDispatchesEvent(t *testing.T) {
	addr := startEmbeddedAnvil(t)
	s := &Server{
		opts:     Options{Protocol: ProtocolIMAP, AnvilAddr: addr},
		sessions: make(map[string][]*liveSession),
	}
	cliEnd, srvEnd := net.Pipe()
	defer cliEnd.Close()
	s.sessions["alice@example.com"] = []*liveSession{{
		id:          "sess-1",
		user:        "alice@example.com",
		backendConn: srvEnd,
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startKickSubscriber(ctx)
	// Give the subscriber a moment to wire up before emitting.
	time.Sleep(100 * time.Millisecond)

	pub, err := anvil.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial pub: %v", err)
	}
	defer pub.Close()
	if err := pub.Emit("kick:imap", "sess-1"); err != nil {
		t.Fatalf("emit: %v", err)
	}

	cliEnd.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := cliEnd.Read(buf); err == nil {
		t.Error("backendConn read returned no error after kick — subscriber did not fire")
	}
}

// TestKickSubscriberReconnectsAfterDrop is the #908 PR3 requirement (the mirror
// of #946 on the subscribe side): after the login→anvil connection is severed,
// the subscriber must redial and re-subscribe so kicks are delivered again. The
// old single-Dial subscriber went permanently deaf on the first drop.
func TestKickSubscriberReconnectsAfterDrop(t *testing.T) {
	anvilAddr := startEmbeddedAnvil(t)
	proxy := newCuttableProxy(t, anvilAddr)

	s := &Server{
		opts:     Options{Protocol: ProtocolIMAP, AnvilAddr: proxy.addr()},
		sessions: make(map[string][]*liveSession),
	}
	cli1, srv1 := net.Pipe()
	defer cli1.Close()
	cli2, srv2 := net.Pipe()
	defer cli2.Close()
	s.sessions["alice@example.com"] = []*liveSession{
		{id: "sess-1", user: "alice@example.com", backendConn: srv1},
		{id: "sess-2", user: "alice@example.com", backendConn: srv2},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startKickSubscriber(ctx)
	time.Sleep(200 * time.Millisecond)

	// Baseline: a kick is delivered through the proxy before any cut.
	emitKick(t, anvilAddr, "sess-1")
	assertConnClosed(t, cli1, "sess-1 before drop")

	// Sever the login→anvil connection. The subscriber's channel closes and the
	// reconnect loop redials + re-subscribes (after kickReconnectDelay).
	proxy.cut()
	// Wait past the backoff plus dial/subscribe round trip.
	time.Sleep(kickReconnectDelay + time.Second)

	emitKick(t, anvilAddr, "sess-2")
	assertConnClosed(t, cli2, "sess-2 after reconnect")
}

// emitKick opens a throwaway anvil conn and emits one kick on the imap channel.
func emitKick(t *testing.T, addr, sessID string) {
	t.Helper()
	pub, err := anvil.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial pub: %v", err)
	}
	defer pub.Close()
	if err := pub.Emit("kick:imap", sessID); err != nil {
		t.Fatalf("emit %s: %v", sessID, err)
	}
}

// assertConnClosed asserts the given pipe end sees its peer closed within 2s.
func assertConnClosed(t *testing.T, c net.Conn, what string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Errorf("%s: backendConn still open — kick not delivered", what)
	}
}
