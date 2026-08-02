package director

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// TestApplyRingChangeVhosts_UpdatesWeightWithoutLease guards #706 item 1 and its
// trap: a replicated vhost change updates the weight and preserves up/down
// state, but must NOT record a lease (no seq) — reweighting a static backend
// must not make it expirable.
func TestApplyRingChangeVhosts_UpdatesWeightWithoutLease(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 100, LastUp: 5})

	applyRingChangeFields(s, []string{"10.0.0.1", "vhosts", "a", "40"})

	b := s.ring.GetBackend("10.0.0.1")
	if b == nil || b.Vhosts != 40 {
		t.Fatalf("vhosts not updated, got %+v", b)
	}
	if !b.Up || b.LastUp != 5 {
		t.Fatalf("up/down state must be preserved, got %+v", b)
	}
	// The trap: no lease was recorded, so the backend stays non-expirable.
	s.backendSeenMu.Lock()
	_, leased := s.backendSeen["10.0.0.1"]
	s.backendSeenMu.Unlock()
	if leased {
		t.Fatal("a vhosts update must NOT make the backend lease-managed")
	}
}

func TestApplyRingChangeVhosts_UnknownBackend(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	// Must not panic; nothing to update.
	applyRingChangeFields(s, []string{"10.9.9.9", "vhosts", "a", "40"})
}

// TestHostLineVhostsRoundTrip: the handshake HOST line carries vhosts, and a
// joining director recovers it — while an old-form line (no V field) still
// parses (Vhosts 0 = ring default).
func TestHostLineVhostsRoundTrip(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	line := hostLine(ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Vhosts: 55, LastUp: 9})
	applyHandshakeHost(s, line)
	if b := s.ring.GetBackend("10.0.0.2"); b == nil || b.Vhosts != 55 {
		t.Fatalf("vhosts not recovered from HOST line, got %+v", b)
	}

	// Pre-#706 form without the trailing V field must still parse.
	applyHandshakeHost(s, "HOST\t10.0.0.3\t10143\ta\tD0\tU9\thost3")
	if b := s.ring.GetBackend("10.0.0.3"); b == nil || b.Vhosts != 0 {
		t.Fatalf("old-form HOST line must parse with Vhosts 0, got %+v", b)
	}
}
