package director

import (
	"fmt"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// evacHarness opens n single-session users pinned to backendIP and returns the server
// plus the user→session-id map so a test can close sessions to confirm kills.
func evacHarness(t *testing.T, n, maxParallel int) (*Server, string, map[string]string) {
	t.Helper()
	s := NewWithOptions(Options{
		MaxParallelMoves:     maxParallel,
		UserKillConfirmGrace: 10 * time.Millisecond,
		UserKillTimeout:      10 * time.Second,
	})
	ip := "10.0.0.1"
	s.ring.AddBackend(&ring.Backend{IP: ip, Port: 993, Tag: "imap", Up: true, Vhosts: 100})

	sessOf := make(map[string]string, n)
	for i := 0; i < n; i++ {
		user := fmt.Sprintf("u%d@d.test", i)
		id := fmt.Sprintf("s%d", i)
		c := &client{conn: &captureConn{}}
		s.handleSessionOpen(c, []string{"SESSION-OPEN", id, user, ip, "imap"})
		sessOf[user] = id
	}
	return s, ip, sessOf
}

func inflightCount(s *Server, ip string) int {
	s.evacMu.Lock()
	defer s.evacMu.Unlock()
	if e := s.evac[ip]; e != nil {
		return len(e.inflight)
	}
	return 0
}

func evacActive(s *Server, ip string) bool {
	s.evacMu.Lock()
	defer s.evacMu.Unlock()
	_, ok := s.evac[ip]
	return ok
}

// confirmInflight closes the sessions of every user currently in flight, then runs the
// sweep past the grace so their kills confirm and the next wave is pulled in.
func confirmInflight(t *testing.T, s *Server, ip string, sessOf map[string]string, grace time.Duration) int {
	t.Helper()
	// Snapshot the in-flight hashes, then find each one's user + session.
	s.evacMu.Lock()
	hashes := make(map[uint32]bool)
	if e := s.evac[ip]; e != nil {
		for h := range e.inflight {
			hashes[h] = true
		}
	}
	s.evacMu.Unlock()

	closed := 0
	for user, id := range sessOf {
		if hashes[HashUsername(user, s.hf)] {
			c := &client{conn: &captureConn{}}
			s.handleSessionClose(c, []string{"SESSION-CLOSE", id})
			closed++
		}
	}
	time.Sleep(grace + 20*time.Millisecond)
	s.sweepKills(grace)
	return closed
}

// TestEvacuation_ThrottlesToWindow proves the core #849 invariant: a graceful drain
// keeps at most max_parallel users in flight at once, draining the rest in self-clocked
// waves as each kill confirms — never a mass simultaneous kick.
func TestEvacuation_ThrottlesToWindow(t *testing.T) {
	const n, window = 5, 2
	grace := 10 * time.Millisecond
	s, ip, sessOf := evacHarness(t, n, window)

	queued := s.startEvacuation(ip, window)
	if queued != n {
		t.Fatalf("queued = %d, want %d", queued, n)
	}
	if got := inflightCount(s, ip); got != window {
		t.Fatalf("initial in-flight = %d, want the window %d", got, window)
	}

	drained := 0
	for evacActive(s, ip) {
		if got := inflightCount(s, ip); got > window {
			t.Fatalf("in-flight %d exceeded the window %d", got, window)
		}
		drained += confirmInflight(t, s, ip, sessOf, grace)
		if drained > n {
			t.Fatalf("drained %d exceeds %d — runaway", drained, n)
		}
	}
	if drained != n {
		t.Errorf("drained %d users, want %d", drained, n)
	}
}

// TestEvacuation_Unlimited proves negative/0 max drains everyone at once (no throttle).
func TestEvacuation_Unlimited(t *testing.T) {
	const n = 4
	s, ip, _ := evacHarness(t, n, -1) // -1 → maxParallelMoves() returns 0 = unlimited
	s.startEvacuation(ip, s.opts.maxParallelMoves())
	if got := inflightCount(s, ip); got != n {
		t.Fatalf("unlimited window in-flight = %d, want all %d at once", got, n)
	}
}

// TestEvacuation_NoSessionsCompletesImmediately: draining a backend with no active
// sessions clears its pins and finishes without any in-flight state.
func TestEvacuation_NoSessionsCompletesImmediately(t *testing.T) {
	s, ip, _ := evacHarness(t, 0, 2)
	if q := s.startEvacuation(ip, 2); q != 0 {
		t.Fatalf("queued = %d, want 0", q)
	}
	if evacActive(s, ip) {
		t.Error("an empty evacuation must not linger as active state")
	}
}
