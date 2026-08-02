package lmtp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/warden"
)

// startTestWarden spins an in-process warden server on a random
// port and returns its address. The server stops when the test
// finishes.
func startTestWarden(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	srv := warden.NewServer(0) // unlimited per-IP — we only test LOOKUP/CONNECT semantics
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.ListenAndServe(ctx, addr, nil)
		close(done)
	}()
	// Wait for the server to come up.
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

// TestWardenSessionClient_ReserveAndRelease covers the happy
// path: first reserve passes LOOKUP+CONNECT, the count visible
// to a second client is now 1, release brings it back to 0.
func TestWardenSessionClient_ReserveAndRelease(t *testing.T) {
	addr := startTestWarden(t)
	c := newWardenSessionClient(addr, 10, "10.0.0.1")
	defer c.releaseAll()

	id, err := c.reserveDelivery("alice@example.com")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if id == "" {
		t.Error("reserve returned empty id")
	}

	// Side-channel: open another client, run LOOKUP — must see 1.
	probe, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	defer probe.Close()
	count, err := probe.Lookup("alice@example.com", "lmtp")
	if err != nil {
		t.Fatalf("probe lookup: %v", err)
	}
	if count != 1 {
		t.Errorf("count after reserve = %d, want 1", count)
	}

	c.releaseDelivery(id)
	count, err = probe.Lookup("alice@example.com", "lmtp")
	if err != nil {
		t.Fatalf("probe lookup after release: %v", err)
	}
	if count != 0 {
		t.Errorf("count after release = %d, want 0", count)
	}
}

// TestWardenSessionClient_RejectsAtLimit confirms the
// cluster-wide cap: with limit=2, the third reserve for the
// same user fails with ErrTooManyConcurrent.
func TestWardenSessionClient_RejectsAtLimit(t *testing.T) {
	addr := startTestWarden(t)

	// Two pre-existing CONNECTs from a sibling pod, simulated
	// by a direct warden client.
	other, err := warden.Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("other dial: %v", err)
	}
	defer other.Close()
	for i, id := range []string{"sib1", "sib2"} {
		if err := other.Connect(id, "bob@example.com", "10.0.0.2", "lmtp"); err != nil {
			t.Fatalf("sibling connect %d: %v", i, err)
		}
	}

	// Now a session client with limit=2 tries to reserve — count
	// is already 2, so ErrTooManyConcurrent.
	c := newWardenSessionClient(addr, 2, "10.0.0.3")
	defer c.releaseAll()
	if _, err := c.reserveDelivery("bob@example.com"); err != ErrTooManyConcurrent {
		t.Fatalf("reserve at limit: got %v, want ErrTooManyConcurrent", err)
	}

	// Drop one sibling — reserve now passes.
	_ = other.Disconnect("sib1", "bob@example.com", "10.0.0.2", "lmtp")
	if _, err := c.reserveDelivery("bob@example.com"); err != nil {
		t.Errorf("reserve after release: %v", err)
	}
}

// TestWardenSessionClient_UnlimitedSkipsLookup verifies that
// limit=-1 (unlimited) still issues CONNECT — so `who` sees the
// delivery — but does not gate on LOOKUP.
func TestWardenSessionClient_UnlimitedSkipsLookup(t *testing.T) {
	addr := startTestWarden(t)
	c := newWardenSessionClient(addr, -1, "10.0.0.4")
	defer c.releaseAll()

	for i := 0; i < 50; i++ {
		if _, err := c.reserveDelivery("carol@example.com"); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	probe, _ := warden.Dial(addr, nil, time.Second)
	defer probe.Close()
	count, _ := probe.Lookup("carol@example.com", "lmtp")
	if count != 50 {
		t.Errorf("count = %d, want 50", count)
	}
}

// TestWardenSessionClient_ReleaseAllIdempotent covers the
// Logout / Reset path: releaseAll fires DISCONNECT for every
// outstanding entry, and a second call is a safe no-op.
func TestWardenSessionClient_ReleaseAllIdempotent(t *testing.T) {
	addr := startTestWarden(t)
	c := newWardenSessionClient(addr, 10, "10.0.0.5")
	for _, u := range []string{"u1@x", "u2@x", "u3@x"} {
		if _, err := c.reserveDelivery(u); err != nil {
			t.Fatalf("reserve %s: %v", u, err)
		}
	}
	c.releaseAll()
	c.releaseAll() // idempotent

	probe, _ := warden.Dial(addr, nil, time.Second)
	defer probe.Close()
	for _, u := range []string{"u1@x", "u2@x", "u3@x"} {
		count, _ := probe.Lookup(u, "lmtp")
		if count != 0 {
			t.Errorf("%s count after releaseAll = %d, want 0", u, count)
		}
	}
}

// TestWardenSessionClient_UnreachableDowngrades covers the
// resilience path: when WardenAddr is set but the server is down,
// reserve returns ErrWardenUnavailable so the caller can downgrade
// to best-effort delivery.
func TestWardenSessionClient_UnreachableDowngrades(t *testing.T) {
	c := newWardenSessionClient("127.0.0.1:1", 10, "10.0.0.6")
	defer c.releaseAll()
	_, err := c.reserveDelivery("alice@example.com")
	if err == nil {
		t.Fatal("expected error on unreachable warden")
	}
}
