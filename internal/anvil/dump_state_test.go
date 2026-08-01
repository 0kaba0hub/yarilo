package anvil

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestStateBackendDumpContract checks Dump against both backends: a connected
// session shows counter==live (no drift), and a penalty shows its count.
func TestStateBackendDumpContract(t *testing.T) {
	backends := map[string]func(t *testing.T) StateBackend{
		"memory": func(*testing.T) StateBackend { return newMemoryBackend(time.Minute, time.Minute, 5) },
		"redis": func(t *testing.T) StateBackend {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			return NewRedisBackend(rdb, "test:anvil:", "test:anvil:events:", time.Minute, time.Minute, 5)
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			defer b.Close()
			const u, ip = "alice@example.com", "1.2.3.4"

			if ok, err := b.SessionConnect("s1", u, ip, "imap"); err != nil || !ok {
				t.Fatalf("connect: (%v,%v)", ok, err)
			}
			b.PenaltyUpdate("9.9.9.9", 3)

			d, err := b.Dump()
			if err != nil {
				t.Fatalf("dump: %v", err)
			}
			var found bool
			for _, c := range d.Counters {
				if c.UserIP == u+"@"+ip {
					found = true
					if c.Counter != 1 || c.Live != 1 {
						t.Fatalf("counter=%d live=%d, want 1/1 (no drift)", c.Counter, c.Live)
					}
				}
			}
			if !found {
				t.Fatalf("counter for %s@%s not in dump: %+v", u, ip, d.Counters)
			}
			var pfound bool
			for _, p := range d.Penalties {
				if p.IP == "9.9.9.9" {
					pfound = true
					if p.Count != 3 {
						t.Fatalf("penalty count=%d, want 3", p.Count)
					}
				}
			}
			if !pfound {
				t.Fatalf("penalty for 9.9.9.9 not in dump: %+v", d.Penalties)
			}
		})
	}
}

// TestRedisDumpShowsDrift proves Dump surfaces a leaked counter: sessions expire
// by TTL (pod crash) without a DISCONNECT, so the counter stays above the live
// tally until reconcile — exactly what an operator inspects.
func TestRedisDumpShowsDrift(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	b := NewRedisBackend(rdb, "test:anvil:", "test:anvil:events:", time.Minute, 200*time.Millisecond, 5)
	const u, ip = "bob@example.com", "5.6.7.8"

	for _, id := range []string{"a", "b"} {
		if ok, err := b.SessionConnect(id, u, ip, "imap"); err != nil || !ok {
			t.Fatalf("connect %s: (%v,%v)", id, ok, err)
		}
	}
	// Sessions expire (crash, no DISCONNECT); the counter is left leaked.
	mr.FastForward(300 * time.Millisecond)

	d, err := b.Dump()
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	var drift int
	for _, c := range d.Counters {
		if c.UserIP == u+"@"+ip {
			drift = c.Counter - c.Live
		}
	}
	if drift != 2 {
		t.Fatalf("drift = %d, want 2 (leaked counter vs 0 live)", drift)
	}
}

// TestDumpWire covers the DUMP verb end to end: register a session over the
// wire, then Dump() over a client conn and assert the counter is reported.
func TestDumpWire(t *testing.T) {
	ts, addr := startTestServer(t, 5)
	_ = ts
	reg := dialTestConn(t, addr)
	if got := reg.cmd(t, "CONNECT\ts1\tu@d\t10.0.0.1\timap"); got[:2] != "OK" {
		t.Fatalf("CONNECT = %q", got)
	}

	c, err := Dial(addr, nil, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	d, err := c.Dump()
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	var ok bool
	for _, cs := range d.Counters {
		if cs.UserIP == "u@d@10.0.0.1" {
			ok = cs.Counter == 1 && cs.Live == 1
		}
	}
	if !ok {
		t.Fatalf("expected counter u@d@10.0.0.1 = 1/1, got %+v", d.Counters)
	}
}
