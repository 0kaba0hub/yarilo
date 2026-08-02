package director

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// TestResolveUserBackend_Sticky guards #792: the admin resolver must return the
// SAME pod a login LOOKUP would — override → sticky userDir pin → ring hash — so
// a per-user backend-api op hits the user's pinned pod (single-writer).
func TestResolveUserBackend_Sticky(t *testing.T) {
	s := NewWithOptions(Options{})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true})

	// Fresh user: hash fallback, not sticky.
	ip, _, _, sticky := s.resolveUserBackend("fresh@d.test")
	if ip == "" || sticky {
		t.Fatalf("fresh user: want a hashed backend (sticky=false), got ip=%q sticky=%v", ip, sticky)
	}
	hashIP := ip

	// Pin the user to the OTHER backend via userDir; the resolver must honour it.
	other := "10.0.0.1"
	if hashIP == other {
		other = "10.0.0.2"
	}
	s.userDir.Set("fresh@d.test", other+":10143", false)
	ip, _, _, sticky = s.resolveUserBackend("fresh@d.test")
	if ip != other || !sticky {
		t.Fatalf("pinned user: want %s (sticky), got ip=%q sticky=%v", other, ip, sticky)
	}

	// A move (now a normal userDir pin, #708 — no overrides map) re-routes the
	// user to the moved backend.
	s.userDir.Set("fresh@d.test", "10.0.0.1:10143", false)
	ip, _, _, sticky = s.resolveUserBackend("fresh@d.test")
	if ip != "10.0.0.1" || !sticky {
		t.Fatalf("moved user: want 10.0.0.1 (sticky), got ip=%q sticky=%v", ip, sticky)
	}
}

// TestResolveUserBackend_StalePinFallsBack: a pin to a down/removed backend must
// fall back to the ring, not return the dead pod.
func TestResolveUserBackend_StalePinFallsBack(t *testing.T) {
	s := NewWithOptions(Options{})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true})
	s.userDir.Set("u@d.test", "10.9.9.9:10143", false) // not in the ring

	ip, _, _, sticky := s.resolveUserBackend("u@d.test")
	if ip != "10.0.0.1" || sticky {
		t.Fatalf("stale pin: want ring fallback 10.0.0.1 (sticky=false), got ip=%q sticky=%v", ip, sticky)
	}
}

func TestResolveUserBackend_EmptyRing(t *testing.T) {
	s := NewWithOptions(Options{})
	if ip, _, _, _ := s.resolveUserBackend("u@d.test"); ip != "" {
		t.Fatalf("empty ring: want \"\", got %q", ip)
	}
}
