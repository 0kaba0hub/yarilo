package director

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

func TestDeleteByBackend(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	s.userDir.Set("a@d.test", "10.0.0.1:10143", false)
	s.userDir.Set("b@d.test", "10.0.0.2:10143", false)
	s.userDir.Set("c@d.test", "10.0.0.1:10143", false)

	if n := s.userDir.DeleteByBackend("10.0.0.1"); n != 2 {
		t.Fatalf("DeleteByBackend removed %d, want 2", n)
	}
	if s.userDir.Get("a@d.test") != nil || s.userDir.Get("c@d.test") != nil {
		t.Fatal("10.0.0.1 pins must be gone")
	}
	if s.userDir.Get("b@d.test") == nil {
		t.Fatal("10.0.0.2 pin must survive")
	}
}

// TestUserKickedApply_ClearsPin guards #706 item 2a: a replica applying a
// USER-KICKED envelope drops the user's sticky pin, so it won't route the
// kicked user back to the old backend.
func TestUserKickedApply_ClearsPin(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	s.userDir.Set("u@d.test", "10.0.0.1:10143", false)

	s.membership.applyEnvelope("USER-KICKED", []string{s.normalizeUser("u@d.test")}, "10.0.0.99:9102", 1)

	if s.userDir.Get("u@d.test") != nil {
		t.Fatal("USER-KICKED apply must clear the pin")
	}
}

// TestFlushVsDown guards #706 item 2b: a replicated flush clears the backend's
// pins but keeps the backend in the ring (drain); a down removes it outright.
func TestFlushVsDown(t *testing.T) {
	// flush: pin cleared, backend stays (Up=false).
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	s.userDir.Set("u@d.test", "10.0.0.1:10143", false)

	applyRingChangeFields(s, []string{"10.0.0.1", "flush", "a"})
	if s.userDir.Get("u@d.test") != nil {
		t.Fatal("flush must clear the backend's pins")
	}
	if b := s.ring.GetBackend("10.0.0.1"); b == nil {
		t.Fatal("flush must KEEP the backend in the ring (drain)")
	} else if b.Up {
		t.Fatal("flush must mark the backend not-Up")
	}

	// down: backend removed entirely.
	s2 := NewWithOptions(Options{AntiEntropyInterval: -1})
	s2.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	s2.userDir.Set("v@d.test", "10.0.0.2:10143", false)

	applyRingChangeFields(s2, []string{"10.0.0.2", "down", "a"})
	if b := s2.ring.GetBackend("10.0.0.2"); b != nil {
		t.Fatal("down must REMOVE the backend from the ring")
	}
}
