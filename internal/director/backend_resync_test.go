package director

import (
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
)

func TestBackendResync_MergeRules(t *testing.T) {
	t.Run("missed-up heals (lease record, absent locally)", func(t *testing.T) {
		s := NewWithOptions(Options{})
		if !s.applyBackendRecord(backendRecord{ip: "10.0.0.1", port: 993, tag: "imap", vhosts: 100, up: true, seq: 5}) {
			t.Fatal("a lease record for an absent backend must be admitted")
		}
		if !hasBackend(s, "10.0.0.1") {
			t.Error("backend must be in the ring after resync")
		}
	})

	t.Run("stale seq is rejected (no downgrade)", func(t *testing.T) {
		s := NewWithOptions(Options{})
		s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true, Vhosts: 100})
		s.recordBackendSeen("10.0.0.1", 5)
		if s.applyBackendRecord(backendRecord{ip: "10.0.0.1", port: 993, tag: "imap", vhosts: 100, up: true, seq: 3}) {
			t.Error("a record with a seq older than our lease must not apply")
		}
	})

	t.Run("static record applies only if absent", func(t *testing.T) {
		s := NewWithOptions(Options{})
		if !s.applyBackendRecord(backendRecord{ip: "10.0.0.9", port: 993, tag: "imap", vhosts: 100, up: true, seq: 0}) {
			t.Fatal("a static record for an absent backend must be admitted")
		}
		if s.applyBackendRecord(backendRecord{ip: "10.0.0.9", port: 993, tag: "imap", vhosts: 100, up: true, seq: 0}) {
			t.Error("a static record for a present backend must be a no-op")
		}
	})

	t.Run("tombstone blocks resurrection until a newer seq", func(t *testing.T) {
		s := NewWithOptions(Options{})
		s.recordBackendSeen("10.0.0.1", 5)
		s.recordBackendTomb("10.0.0.1") // removed at seq 5
		s.forgetBackendLease("10.0.0.1")
		s.ring.RemoveBackend("10.0.0.1")

		// A snapshot at-or-below the removal seq is a ghost — blocked.
		if s.applyBackendRecord(backendRecord{ip: "10.0.0.1", port: 993, tag: "imap", vhosts: 100, up: true, seq: 5}) {
			t.Error("a record at the removal seq must not resurrect a tombstoned backend")
		}
		if hasBackend(s, "10.0.0.1") {
			t.Fatal("tombstoned backend must stay out of the ring")
		}
		// A static (seq 0) record is likewise blocked while tombstoned.
		if s.applyBackendRecord(backendRecord{ip: "10.0.0.1", port: 993, tag: "imap", vhosts: 100, up: true, seq: 0}) {
			t.Error("a static record must not resurrect a tombstoned backend")
		}
		// A strictly-newer seq is a genuine re-registration — admitted, tomb cleared.
		if !s.applyBackendRecord(backendRecord{ip: "10.0.0.1", port: 993, tag: "imap", vhosts: 100, up: true, seq: 6}) {
			t.Error("a strictly-newer seq must re-admit the backend")
		}
		if !hasBackend(s, "10.0.0.1") {
			t.Error("re-registered backend must be back in the ring")
		}
	})
}

// captureConn is a net.Conn stand-in that records written bytes; used to assert
// onBackendHash emits a BACKEND-SYNC-REQ.
type captureConn struct {
	nopConn
	written []byte
}

func (c *captureConn) Write(b []byte) (int, error) {
	c.written = append(c.written, b...)
	return len(b), nil
}

func TestBackendResync_HashDebounce(t *testing.T) {
	s := NewWithOptions(Options{})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true, Vhosts: 100})
	conn := &captureConn{}

	// A differing hash for ONE tick must not pull yet (debounce = 2).
	s.membership.onBackendHash(conn, "deadbeef")
	if len(conn.written) != 0 {
		t.Fatalf("a single mismatch must not trigger a snapshot pull, got %q", conn.written)
	}
	// Second consecutive mismatch crosses the debounce → BACKEND-SYNC-REQ.
	s.membership.onBackendHash(conn, "deadbeef")
	if got := string(conn.written); got != "BACKEND-SYNC-REQ\n" {
		t.Fatalf("two consecutive mismatches must pull a snapshot, got %q", got)
	}

	// A matching hash resets the streak (no further pulls).
	conn.written = nil
	own := backendSetHash(s.ring.Backends())
	s.membership.onBackendHash(conn, own)
	s.membership.onBackendHash(conn, own)
	if len(conn.written) != 0 {
		t.Errorf("matching hashes must not pull, got %q", conn.written)
	}
}

// TestBackendResync_HealsDivergence is the in-process gate: an N=2 ring where a
// backend was added to only ONE director (a dropped RING-CHANGE) self-heals —
// the other director pulls a snapshot on the sustained hash mismatch and
// converges on the same backend set.
func TestBackendResync_HealsDivergence(t *testing.T) {
	interval := 200 * time.Millisecond
	sA, addrA := startRingNodeAE(t, "shared-secret", nil, 2, interval)
	sB, _ := startRingNodeAE(t, "shared-secret", []string{addrA}, 2, interval)
	waitFor(t, 5*time.Second, func() bool {
		return len(sA.membership.Members()) == 2 && len(sB.membership.Members()) == 2
	})

	// Inject a backend into A ONLY, as if B dropped the RING-CHANGE up. A holds
	// a lease seq for it; B knows nothing.
	sA.ring.AddBackend(&ring.Backend{IP: "10.9.9.9", Port: 993, Tag: "imap", Up: true, Vhosts: 100})
	sA.recordBackendSeen("10.9.9.9", 1)

	waitFor(t, 5*time.Second, func() bool {
		return hasBackend(sB, "10.9.9.9")
	})
	if backendSetHash(sA.ring.Backends()) != backendSetHash(sB.ring.Backends()) {
		t.Error("both directors must converge on the same backend-set hash after resync")
	}
}
