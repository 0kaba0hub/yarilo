package anvil

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestStateBackendPenaltyContract runs the shared penalty contract against both
// backends (#908), so memory and Redis agree on the behaviour the server relies
// on: miss on an unknown IP, hit after an update, and clear on count 0.
func TestStateBackendPenaltyContract(t *testing.T) {
	const decay = 100 * time.Millisecond
	backends := map[string]func(t *testing.T) StateBackend{
		"memory": func(*testing.T) StateBackend { return newMemoryBackend(decay) },
		"redis": func(t *testing.T) StateBackend {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			return NewRedisBackend(rdb, "test:anvil:", decay)
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			defer b.Close()

			if c, s := b.PenaltyLookup("1.2.3.4"); c != 0 || s != "miss" {
				t.Fatalf("empty lookup = (%d,%q), want (0,miss)", c, s)
			}
			b.PenaltyUpdate("1.2.3.4", 3)
			if c, s := b.PenaltyLookup("1.2.3.4"); c != 3 || s != "hit" {
				t.Fatalf("after update = (%d,%q), want (3,hit)", c, s)
			}
			// A second IP is independent.
			if c, _ := b.PenaltyLookup("5.6.7.8"); c != 0 {
				t.Fatalf("unrelated IP = %d, want 0", c)
			}
			// Count 0 clears the entry.
			b.PenaltyUpdate("1.2.3.4", 0)
			if c, s := b.PenaltyLookup("1.2.3.4"); c != 0 || s == "hit" {
				t.Fatalf("after clear = (%d,%q), want count 0 and not hit", c, s)
			}
		})
	}
}

// TestMemoryPenaltyExpiry: the memory backend evicts on read past the decay
// window and reports "expired".
func TestMemoryPenaltyExpiry(t *testing.T) {
	b := newMemoryBackend(40 * time.Millisecond)
	b.PenaltyUpdate("1.2.3.4", 5)
	time.Sleep(60 * time.Millisecond)
	if c, s := b.PenaltyLookup("1.2.3.4"); c != 0 || s != "expired" {
		t.Fatalf("after decay = (%d,%q), want (0,expired)", c, s)
	}
}

// TestMemoryPenaltySweep: the periodic sweep drops entries older than the decay
// window (the sweeper path, distinct from lazy read-eviction).
func TestMemoryPenaltySweep(t *testing.T) {
	b := newMemoryBackend(time.Minute)
	b.PenaltyUpdate("1.2.3.4", 2)
	// Sweep with a clock far past the entry's update time.
	b.PenaltySweep(time.Now().Add(2 * time.Minute))
	if c := len(b.penalties); c != 0 {
		t.Fatalf("sweep left %d entries, want 0", c)
	}
}

// TestRedisPenaltyLookupErrorStatus: a real Redis error (server gone) fails open
// to count 0 but reports "error" — distinct from "miss" — so the outage is
// visible in the penaltyLookups metric rather than hidden as a normal miss.
func TestRedisPenaltyLookupErrorStatus(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), DialTimeout: 500 * time.Millisecond})
	t.Cleanup(func() { rdb.Close() })
	b := NewRedisBackend(rdb, "test:anvil:", time.Minute)

	mr.Close() // Redis is now down.
	if c, s := b.PenaltyLookup("1.2.3.4"); c != 0 || s != "error" {
		t.Fatalf("with Redis down = (%d,%q), want (0,error)", c, s)
	}
}

// TestRedisPenaltyTTL proves the Redis backend sets a key TTL (= decay), so a
// stale penalty expires without a sweeper — the property PR1's gate checks.
func TestRedisPenaltyTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	b := NewRedisBackend(rdb, "test:anvil:", 100*time.Millisecond)

	b.PenaltyUpdate("1.2.3.4", 4)
	if c, _ := b.PenaltyLookup("1.2.3.4"); c != 4 {
		t.Fatalf("before TTL = %d, want 4", c)
	}
	mr.FastForward(200 * time.Millisecond)
	if c, s := b.PenaltyLookup("1.2.3.4"); c != 0 || s != "miss" {
		t.Fatalf("after TTL = (%d,%q), want (0,miss) — key should have expired", c, s)
	}
}
