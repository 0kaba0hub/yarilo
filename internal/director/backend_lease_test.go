package director

import (
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

func hasBackend(s *Server, ip string) bool {
	return s.ring.GetBackend(ip) != nil
}

// TestBackendLease_SeqFreshness: only a strictly newer per-origin seq
// refreshes the lease; a stale/duplicate seq does not (#776).
func TestBackendLease_SeqFreshness(t *testing.T) {
	s := NewWithOptions(Options{})
	if !s.recordBackendSeen("10.0.0.1", 5) {
		t.Fatal("first heartbeat must be fresh")
	}
	if s.recordBackendSeen("10.0.0.1", 5) {
		t.Fatal("duplicate seq must not be fresh")
	}
	if s.recordBackendSeen("10.0.0.1", 4) {
		t.Fatal("older seq must not be fresh")
	}
	if !s.recordBackendSeen("10.0.0.1", 6) {
		t.Fatal("newer seq must be fresh")
	}
}

// TestBackendLease_ExpiresStaleHeartbeat: a lease-managed backend that stops
// heartbeating is removed once its last-seen exceeds BackendExpire; a second
// (static) backend keeps the tag alive so the guard does not block removal.
func TestBackendLease_ExpiresStaleHeartbeat(t *testing.T) {
	s := NewWithOptions(Options{BackendExpire: 50 * time.Millisecond})
	// A static (non-lease) sibling so the expiring one is not the last of the tag.
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "imap", Up: true})
	// The heartbeating one.
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "imap", Up: true})
	s.recordBackendSeen("10.0.0.1", 1)

	time.Sleep(80 * time.Millisecond)
	s.expireStaleBackends(50 * time.Millisecond)

	if hasBackend(s, "10.0.0.1") {
		t.Error("stale lease-managed backend must be removed")
	}
	if !hasBackend(s, "10.0.0.2") {
		t.Error("static backend (no lease) must never be expired")
	}
}

// TestBackendLease_RefreshedHeartbeatSurvives: a backend that keeps
// heartbeating is not expired.
func TestBackendLease_RefreshedHeartbeatSurvives(t *testing.T) {
	s := NewWithOptions(Options{BackendExpire: 100 * time.Millisecond})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "imap", Up: true})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "imap", Up: true})
	s.recordBackendSeen("10.0.0.1", 1)
	s.recordBackendSeen("10.0.0.2", 1)

	// Keep .1 fresh across an expire window; .2 goes stale.
	time.Sleep(60 * time.Millisecond)
	s.recordBackendSeen("10.0.0.1", 2)
	time.Sleep(60 * time.Millisecond)
	s.expireStaleBackends(100 * time.Millisecond)

	if !hasBackend(s, "10.0.0.1") {
		t.Error("continuously-heartbeating backend must survive")
	}
	if hasBackend(s, "10.0.0.2") {
		t.Error("silent backend must be expired")
	}
}

// TestBackendLease_NeverExpiresLastOfTag: the last backend of a tag is kept
// even when its lease is stale — a suspect-but-only backend beats a total
// blackhole.
func TestBackendLease_NeverExpiresLastOfTag(t *testing.T) {
	s := NewWithOptions(Options{BackendExpire: 50 * time.Millisecond})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "imap", Up: true})
	s.recordBackendSeen("10.0.0.1", 1)

	time.Sleep(80 * time.Millisecond)
	s.expireStaleBackends(50 * time.Millisecond)

	if !hasBackend(s, "10.0.0.1") {
		t.Error("the last backend of a tag must never be expired")
	}
}

// TestBackendLease_StaticNeverManaged: a backend that never heartbeats is
// not in the lease map and is never expired even with no siblings.
func TestBackendLease_StaticNeverManaged(t *testing.T) {
	s := NewWithOptions(Options{BackendExpire: 20 * time.Millisecond})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.9", Port: 10143, Tag: "imap", Up: true})
	time.Sleep(50 * time.Millisecond)
	s.expireStaleBackends(20 * time.Millisecond)
	if !hasBackend(s, "10.0.0.9") {
		t.Error("a non-heartbeating (static/admin) backend must never be lease-expired")
	}
}

// TestBackendLease_GossipedHeartbeatRefreshes: a heartbeat that lands on ANY
// director (here applied as the gossiped RING-CHANGE up with a seq) refreshes
// this director's lease — the property that makes a load-balanced heartbeat
// correct (#776).
func TestBackendLease_GossipedHeartbeatRefreshes(t *testing.T) {
	s := NewWithOptions(Options{})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "imap", Up: true})

	// A gossiped RING-CHANGE up carrying the backend's seq (4th field).
	applyRingChangeFields(s, []string{"10.0.0.1", "up", "imap", "7"})

	s.backendSeenMu.Lock()
	lease, ok := s.backendSeen["10.0.0.1"]
	s.backendSeenMu.Unlock()
	if !ok || lease.seq != 7 {
		t.Fatalf("gossiped heartbeat must refresh the local lease, got %+v ok=%v", lease, ok)
	}
}
