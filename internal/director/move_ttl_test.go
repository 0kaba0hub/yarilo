package director

import (
	"testing"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

func TestDeleteIfBackend(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	s.userDir.Set("u@d.test", "10.0.0.1:10143", false)

	// Wrong backend → no delete.
	if s.userDir.DeleteIfBackend("u@d.test", "10.0.0.9") {
		t.Fatal("must not delete a pin that points elsewhere")
	}
	if s.userDir.Get("u@d.test") == nil {
		t.Fatal("pin must survive a non-matching DeleteIfBackend")
	}
	// Matching backend → delete.
	if !s.userDir.DeleteIfBackend("u@d.test", "10.0.0.1") {
		t.Fatal("must delete a pin that points at the given backend")
	}
	if s.userDir.Get("u@d.test") != nil {
		t.Fatal("pin must be gone after a matching DeleteIfBackend")
	}
}

// TestMoveUser_CompareAndDeleteKick guards #708: a move writes the new pin and
// its trailing kick (old backend) must NOT delete that fresh pin, while a plain
// admin kick still clears unconditionally (#823 preserved).
func TestMoveUser_CompareAndDeleteKick(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true, Vhosts: 100})

	// User starts pinned to .1.
	s.userDir.Set("u@d.test", "10.0.0.1:10143", false)

	// Move to .2 — sets the new pin and originates USER-MOVED + a conditional
	// USER-KICKED carrying the OLD backend (.1).
	s.moveUser("u@d.test", "10.0.0.2:10143", nil)
	if e := s.userDir.Get("u@d.test"); e == nil || e.Host != "10.0.0.2:10143" {
		t.Fatalf("move must set the new pin, got %+v", e)
	}

	// The move-kick (old=.1) applied on a replica must NOT clear the fresh .2 pin.
	s.membership.applyEnvelope("USER-KICKED", []string{"u@d.test", "10.0.0.1"})
	if e := s.userDir.Get("u@d.test"); e == nil || e.Host != "10.0.0.2:10143" {
		t.Fatalf("move-kick must leave the new pin intact, got %+v", e)
	}

	// A plain admin kick (no old backend) clears unconditionally (#823).
	s.membership.applyEnvelope("USER-KICKED", []string{"u@d.test"})
	if s.userDir.Get("u@d.test") != nil {
		t.Fatal("plain admin kick must clear the pin unconditionally")
	}
}
