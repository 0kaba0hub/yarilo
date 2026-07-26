package director

import (
	"fmt"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

func leastSessionsServer(t *testing.T) *Server {
	t.Helper()
	return NewWithOptions(Options{AssignmentPolicy: "least_sessions", AntiEntropyInterval: -1})
}

// addSess injects an active session into the registry (as SESSION-OPEN would).
func addSess(s *Server, id, backend, proto string) {
	s.sessRecMu.Lock()
	s.sessById[id] = &sessionRec{id: id, backend: backend, proto: proto}
	if s.sessByBE[backend] == nil {
		s.sessByBE[backend] = make(map[string]bool)
	}
	s.sessByBE[backend][id] = true
	s.sessRecMu.Unlock()
}

func TestPickBackend_LeastSessions_Level1Protocol(t *testing.T) {
	s := leastSessionsServer(t)
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	// .1 is heavier on imap; .2 should win an imap placement.
	for i := 0; i < 5; i++ {
		addSess(s, fmt.Sprintf("s%d", i), "10.0.0.1", "imap")
	}
	if b := s.pickBackend("u@d.test", "a", "imap"); b == nil || b.IP != "10.0.0.2" {
		t.Fatalf("level1: want 10.0.0.2 (fewer imap sessions), got %v", b)
	}
}

func TestPickBackend_LeastSessions_Level2Total(t *testing.T) {
	s := leastSessionsServer(t)
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	// Equal imap (0 each) → level 1 ties; .1 heavier on pop3 → level 2 picks .2.
	for i := 0; i < 4; i++ {
		addSess(s, fmt.Sprintf("p%d", i), "10.0.0.1", "pop3")
	}
	if b := s.pickBackend("u@d.test", "a", "imap"); b == nil || b.IP != "10.0.0.2" {
		t.Fatalf("level2: want 10.0.0.2 (fewer total), got %v", b)
	}
}

func TestPickBackend_AdminPath_NoProto_UsesTotal(t *testing.T) {
	s := leastSessionsServer(t)
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	// .2 has many imap but .1 has more TOTAL. With reqProto="" level 1 is
	// skipped, so total decides → .2 (the imap-heavy but total-lighter one).
	addSess(s, "i1", "10.0.0.2", "imap")
	addSess(s, "i2", "10.0.0.2", "imap")
	for i := 0; i < 5; i++ {
		addSess(s, fmt.Sprintf("t%d", i), "10.0.0.1", "pop3")
	}
	if b := s.pickBackend("u@d.test", "a", ""); b == nil || b.IP != "10.0.0.2" {
		t.Fatalf("admin path (no proto): want 10.0.0.2 by total, got %v", b)
	}
}

func TestPickBackend_StrictTag_NoNeighbourFallback(t *testing.T) {
	s := leastSessionsServer(t)
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "b", Up: true, Vhosts: 100})
	// Request tag "a"; only tag "b" exists → must be nil (FAIL), never "b".
	if b := s.pickBackend("u@d.test", "a", "imap"); b != nil {
		t.Fatalf("strict tag: want nil for empty candidate set, got %v", b)
	}
}

func TestPickBackend_VhostsZeroExcluded(t *testing.T) {
	s := leastSessionsServer(t)
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 0})   // drain
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true, Vhosts: 100}) // busy but eligible
	for i := 0; i < 9; i++ {
		addSess(s, fmt.Sprintf("s%d", i), "10.0.0.2", "imap")
	}
	// .1 has zero sessions but is draining → excluded; .2 wins despite load.
	if b := s.pickBackend("u@d.test", "a", "imap"); b == nil || b.IP != "10.0.0.2" {
		t.Fatalf("vhosts=0 must be excluded (drain), got %v", b)
	}
}

func TestPickBackend_VhostsNormalization(t *testing.T) {
	s := leastSessionsServer(t)
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 50})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	// Equal raw count (1 each): normalized load .1 = 100/50 = 2, .2 = 100/100 =
	// 1 → the higher-capacity .2 wins. (vhosts=50 reaches equal load at half the
	// sessions, so it takes ~half the users.)
	addSess(s, "a1", "10.0.0.1", "imap")
	addSess(s, "b1", "10.0.0.2", "imap")
	if b := s.pickBackend("u@d.test", "a", "imap"); b == nil || b.IP != "10.0.0.2" {
		t.Fatalf("vhosts normalization: want 10.0.0.2 (higher capacity), got %v", b)
	}
}

func TestPickBackend_HashPolicyUnchanged(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1}) // default = hash
	for i := 1; i <= 3; i++ {
		s.ring.AddBackend(&ring.Backend{IP: fmt.Sprintf("10.0.0.%d", i), Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	}
	got := s.pickBackend("bob@d.test", "a", "imap")
	want := s.ring.LookupBackendByTag("bob@d.test", "a")
	if got == nil || want == nil || got.IP != want.IP {
		t.Fatalf("hash policy must equal the ring lookup: got %v want %v", got, want)
	}
}

func TestAssignAndPin_RecordsPin(t *testing.T) {
	s := leastSessionsServer(t)
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	b := s.assignAndPin("u@d.test", "a", "imap")
	if b == nil {
		t.Fatal("assignAndPin returned nil")
	}
	e := s.userDir.Get("u@d.test")
	if e == nil || e.Host != "10.0.0.1:10143" {
		t.Fatalf("assignAndPin must record the pin, got %+v", e)
	}
}
