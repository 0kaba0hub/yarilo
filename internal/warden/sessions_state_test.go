package warden

import (
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestStateBackendSessionContract runs the session contract against both backends
// (#908 PR2): the limit is enforced atomically, disconnect frees a slot and is
// idempotent, and touch/list/lookup agree. Memory and Redis must behave the same.
func TestStateBackendSessionContract(t *testing.T) {
	const limit = 2
	backends := map[string]func(t *testing.T) StateBackend{
		"memory": func(*testing.T) StateBackend { return newMemoryBackend(time.Minute, time.Minute, limit) },
		"redis": func(t *testing.T) StateBackend {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			return NewRedisBackend(rdb, "test:warden:", "test:warden:events:", time.Minute, time.Minute, limit)
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			defer b.Close()
			const u, ip = "alice@example.com", "1.2.3.4"

			// Exactly `limit` connects pass; the next is rejected (not an error).
			for i := 1; i <= limit; i++ {
				ok, err := b.SessionConnect(sid(i), u, ip, "imap")
				if err != nil || !ok {
					t.Fatalf("connect %d = (%v,%v), want (true,nil)", i, ok, err)
				}
			}
			if ok, err := b.SessionConnect(sid(limit+1), u, ip, "imap"); ok || err != nil {
				t.Fatalf("over-limit connect = (%v,%v), want (false,nil)", ok, err)
			}
			if got := b.SessionLookupCount(u, "imap"); got != limit {
				t.Fatalf("lookup count = %d, want %d", got, limit)
			}
			if got := b.SessionCount(); got != limit {
				t.Fatalf("session count = %d, want %d", got, limit)
			}

			// Idempotent CONNECT (#942 review): a retry with an already-registered
			// id must NOT take a second slot. Re-CONNECT sid(1); the count is
			// unchanged and the pool stays full.
			if ok, err := b.SessionConnect(sid(1), u, ip, "imap"); err != nil || !ok {
				t.Fatalf("idempotent re-connect = (%v,%v), want (true,nil)", ok, err)
			}
			if got := b.SessionCount(); got != limit {
				t.Fatalf("count after idempotent re-connect = %d, want %d (no double-count)", got, limit)
			}

			// Touch: known vs unknown.
			if !b.SessionTouch(sid(1)) {
				t.Fatal("touch of a live session should be known")
			}
			if b.SessionTouch("ghost") {
				t.Fatal("touch of an unknown session should be false")
			}

			// Disconnect frees a slot; a duplicate disconnect must NOT free a
			// second (idempotent) — otherwise the counter drifts negative and the
			// limit is over-permissive.
			b.SessionDisconnect(sid(1), u, ip)
			b.SessionDisconnect(sid(1), u, ip)
			ok, err := b.SessionConnect(sid(98), u, ip, "imap")
			if err != nil || !ok {
				t.Fatalf("connect after one free = (%v,%v), want (true,nil)", ok, err)
			}
			// Only one slot was freed, so the pool is full again.
			if ok, _ := b.SessionConnect(sid(99), u, ip, "imap"); ok {
				t.Fatal("a duplicate disconnect leaked a second slot")
			}
		})
	}
}

// TestRedisSessionReconcile is the crash-recovery proof (#908 PR2): a login pod's
// sessions expire by TTL when it dies, but their INCR was never matched by a
// DECR, so the counter leaks. Maintain must reconcile it back down — race-safely
// — so users are not locked out by a phantom count.
func TestRedisSessionReconcile(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	const limit = 3
	b := NewRedisBackend(rdb, "test:warden:", "test:warden:events:", time.Minute, 200*time.Millisecond, limit)
	const u, ip = "alice@example.com", "1.2.3.4"

	for i := 1; i <= limit; i++ {
		if ok, err := b.SessionConnect(sid(i), u, ip, "imap"); err != nil || !ok {
			t.Fatalf("connect %d failed: (%v,%v)", i, ok, err)
		}
	}
	// The pod "crashes": no DISCONNECTs. The session keys expire by TTL, but the
	// counter is left at the limit — a leak that would lock the user out.
	mr.FastForward(300 * time.Millisecond)
	if got := b.SessionCount(); got != 0 {
		t.Fatalf("sessions after TTL = %d, want 0 (keys expired)", got)
	}
	if ok, _ := b.SessionConnect(sid(50), u, ip, "imap"); ok {
		t.Fatal("expected the leaked counter to still block a connect before reconcile")
	}

	// Reconciliation corrects the leaked counter from the (now empty) live set.
	b.Maintain(time.Now())

	if ok, err := b.SessionConnect(sid(51), u, ip, "imap"); err != nil || !ok {
		t.Fatalf("connect after reconcile = (%v,%v), want (true,nil) — leak not cleared", ok, err)
	}
}

func sid(n int) string { return "sess-" + strconv.Itoa(n) }
