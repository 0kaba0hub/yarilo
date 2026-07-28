package login

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestServer_ShutdownStopsAccepting verifies the #857 graceful drain: Shutdown
// closes the listener (so Serve returns nil, a clean stop) and refuses new
// connections afterwards.
func TestServer_ShutdownStopsAccepting(t *testing.T) {
	s := New(Options{Protocol: ProtocolIMAP})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ln) }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve after Shutdown = %v, want nil (clean stop)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("listener must be closed after Shutdown — new connections refused")
	}
	// Second Shutdown is a no-op.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}
