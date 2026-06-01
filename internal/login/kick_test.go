package login

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/anvil"
)

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
