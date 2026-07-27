package director

import (
	"net"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

// twoBackendRing returns a director whose ring has two backends of the same tag
// (so the last-of-tag guard never blocks an eviction), with the given
// corroboration threshold and window.
func twoBackendRing(t *testing.T, reporters int, window time.Duration) *Server {
	t.Helper()
	s := NewWithOptions(Options{UnreachableReporters: reporters, UnreachableWindow: window})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "imap", Up: true})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 10143, Tag: "imap", Up: true})
	return s
}

func TestUnreachable_EvictsOnCorroboratedReports(t *testing.T) {
	s := twoBackendRing(t, 2, 5*time.Second)
	now := time.Now()

	if s.recordUnreachable("10.0.0.1", "proxy-a", now) {
		t.Fatal("one distinct reporter must not reach the threshold of 2")
	}
	if !s.recordUnreachable("10.0.0.1", "proxy-b", now) {
		t.Fatal("two distinct reporters within the window must reach the threshold")
	}
	s.evictUnreachable("10.0.0.1", nil)
	if hasBackend(s, "10.0.0.1") {
		t.Error("a corroborated-unreachable backend must be evicted from the ring")
	}
	if !hasBackend(s, "10.0.0.2") {
		t.Error("the sibling backend must stay")
	}
}

func TestUnreachable_SameReporterCountsOnce(t *testing.T) {
	s := twoBackendRing(t, 2, 5*time.Second)
	now := time.Now()
	// The same proxy reporting twice is still one distinct reporter.
	s.recordUnreachable("10.0.0.1", "proxy-a", now)
	if s.recordUnreachable("10.0.0.1", "proxy-a", now.Add(time.Second)) {
		t.Fatal("repeated reports from one reporter must not reach a threshold of 2")
	}
}

func TestUnreachable_StaleReportsPrunedByWindow(t *testing.T) {
	s := twoBackendRing(t, 2, 5*time.Second)
	base := time.Now()
	// proxy-a reports, then more than a window later proxy-b reports: proxy-a
	// is stale and pruned, so only one distinct reporter remains in-window.
	s.recordUnreachable("10.0.0.1", "proxy-a", base)
	if s.recordUnreachable("10.0.0.1", "proxy-b", base.Add(6*time.Second)) {
		t.Fatal("reports spread beyond the window must not corroborate")
	}
}

func TestUnreachable_NeverEvictsLastOfTag(t *testing.T) {
	s := NewWithOptions(Options{UnreachableReporters: 1, UnreachableWindow: 5 * time.Second})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 10143, Tag: "imap", Up: true})

	s.recordUnreachable("10.0.0.1", "proxy-a", time.Now())
	s.evictUnreachable("10.0.0.1", nil)
	if !hasBackend(s, "10.0.0.1") {
		t.Error("the last backend of a tag must never be evicted, even when reported unreachable")
	}
}

func TestUnreachable_ReadmitClearsReports(t *testing.T) {
	s := twoBackendRing(t, 2, 5*time.Second)
	now := time.Now()
	s.recordUnreachable("10.0.0.1", "proxy-a", now)

	// A fresh heartbeat re-admits the backend and must wipe the stale report,
	// so a single later report does not immediately re-cross the threshold.
	s.handleBackendUp(&client{conn: nopConn{}}, []string{"BACKEND-UP", "10.0.0.1", "10143", "imap"})

	if s.recordUnreachable("10.0.0.1", "proxy-b", now.Add(time.Second)) {
		t.Fatal("after re-admit the earlier report must be cleared, so one new report cannot corroborate")
	}
}

// TestUnreachable_RingGossipAggregates verifies the #804 guard: reports that
// arrive over the ring (from proxies that reached OTHER directors) count toward
// the same distinct-reporter total, so the threshold is reachable even though
// each director sees only some reports directly.
func TestUnreachable_RingGossipAggregates(t *testing.T) {
	s := twoBackendRing(t, 2, 5*time.Second)
	// One report gossiped from a peer (proxy-a reached another director), one
	// local (proxy-b). Two distinct reporters -> evict.
	s.applyRemoteUnreachable([]string{"10.0.0.1", "proxy-a"})
	if hasBackend(s, "10.0.0.1") {
		// still up: only one distinct reporter so far
		s.applyRemoteUnreachable([]string{"10.0.0.1", "proxy-b"})
	}
	if hasBackend(s, "10.0.0.1") {
		t.Error("two distinct reporters (one gossiped, one local) must evict the backend")
	}
}

// nopConn is a minimal net.Conn stand-in so handleBackendUp's client has a
// non-nil conn (RemoteAddr is not exercised on this path).
type nopConn struct{ net.Conn }

func (nopConn) Write(b []byte) (int, error) { return len(b), nil }
