package director

import (
	"testing"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// TestMapPeekVsResolver guards the #813 split: introspection (peek =
// userDir.Get) must NOT create a pin, while the routing resolver
// (resolveUserBackend) still pins an unpinned user under least_sessions
// (#792/#797 — admin per-user ops depend on it).
func TestMapPeekVsResolver(t *testing.T) {
	s := leastSessionsServer(t)
	// Untagged pool: the admin resolve path uses tag "" (strict match).
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "", Up: true, Vhosts: 100})
	user := s.normalizeUser("u@d.test")

	// peek: Get returns nil for an unpinned user and leaves the map empty.
	if e := s.userDir.Get(user); e != nil {
		t.Fatalf("peek: unpinned user must be nil, got %+v", e)
	}
	if n := len(s.userDir.Snapshot()); n != 0 {
		t.Fatalf("peek must not create a pin, snapshot has %d entries", n)
	}

	// resolver: pins the user (side effect is intended here).
	if ip, _, _, _ := s.resolveUserBackend(user); ip == "" {
		t.Fatal("resolver must place an unpinned user under least_sessions")
	}
	if e := s.userDir.Get(user); e == nil {
		t.Fatal("resolver must have pinned the user")
	}
}
