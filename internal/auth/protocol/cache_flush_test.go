package protocol

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// startMasterWithCache mirrors startMaster but threads a Cache
// through WithMasterCache so CACHE-FLUSH is functional.
func startMasterWithCache(t *testing.T, cache *Cache) func() (net.Conn, *bufio.Reader) {
	t.Helper()
	srv := NewMasterServer(nil, WithMasterCache(cache))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Cleanup(cancel)
	return func() (net.Conn, *bufio.Reader) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn, bufio.NewReader(conn)
	}
}

func drainMasterHandshake(t *testing.T, rd *bufio.Reader) {
	t.Helper()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake: %v", err)
		}
		if strings.TrimSpace(line) == "DONE" {
			return
		}
	}
}

// TestMaster_CacheFlush_NoCacheConfigured — CACHE-FLUSH on a
// MasterServer constructed without WithMasterCache must reject
// with a descriptive FAIL (not a silent OK 0).
func TestMaster_CacheFlush_NoCacheConfigured(t *testing.T) {
	dial := startMaster(t, nil) // helper from master_test.go — no cache
	conn, rd := dial()
	drainMasterHandshake(t, rd)

	fmt.Fprintf(conn, "CACHE-FLUSH\t1\n")
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(line, "FAIL\t1\treason=no cache") {
		t.Errorf("unexpected reply: %q", line)
	}
}

// TestMaster_CacheFlush_FullFlush — CACHE-FLUSH without masks
// empties the entire cache.
func TestMaster_CacheFlush_FullFlush(t *testing.T) {
	cache := NewCache(1<<20, time.Minute, time.Minute)
	cache.Insert(MakeCacheKey("imap", "alice"), "alice", "p", ResultOK, NewFields())
	cache.Insert(MakeCacheKey("imap", "bob"), "bob", "p", ResultOK, NewFields())

	dial := startMasterWithCache(t, cache)
	conn, rd := dial()
	drainMasterHandshake(t, rd)

	fmt.Fprintf(conn, "CACHE-FLUSH\t2\n")
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(line, "OK\t2\t2\n") {
		t.Errorf("expected OK\\t2\\t2, got %q", line)
	}
	_, _, _, n := cache.Stats()
	if n != 0 {
		t.Errorf("cache not empty after flush: %d entries", n)
	}
}

// TestMaster_CacheFlush_ByMask — CACHE-FLUSH with masks evicts
// only matching entries.
func TestMaster_CacheFlush_ByMask(t *testing.T) {
	cache := NewCache(1<<20, time.Minute, time.Minute)
	cache.Insert(MakeCacheKey("imap", "alice@a.com"), "alice@a.com", "p", ResultOK, NewFields())
	cache.Insert(MakeCacheKey("imap", "alice@b.com"), "alice@b.com", "p", ResultOK, NewFields())
	cache.Insert(MakeCacheKey("imap", "bob@a.com"), "bob@a.com", "p", ResultOK, NewFields())

	dial := startMasterWithCache(t, cache)
	conn, rd := dial()
	drainMasterHandshake(t, rd)

	fmt.Fprintf(conn, "CACHE-FLUSH\t3\talice@*\n")
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(line, "OK\t3\t2\n") {
		t.Errorf("expected OK\\t3\\t2, got %q", line)
	}
	if _, ok := cache.Lookup(MakeCacheKey("imap", "alice@a.com"), "p"); ok {
		t.Errorf("alice@a.com still cached after mask flush")
	}
	if _, ok := cache.Lookup(MakeCacheKey("imap", "bob@a.com"), "p"); !ok {
		t.Errorf("bob@a.com lost after alice@* flush")
	}
}

// TestServer_Cache_HitAvoidsChain — wire Server's handleAuth
// honours the attached cache: a primed entry short-circuits the
// passdb chain entirely.
func TestServer_Cache_HitAvoidsChain(t *testing.T) {
	cache := NewCache(1<<20, time.Minute, time.Minute)
	// Prime the cache directly with a known-good entry for alice.
	primed := NewFields()
	primed.Set("user", "alice")
	primed.Set("home", "/mail/alice")
	cache.Insert(MakeCacheKey("imap", "alice"), "alice", "secret", ResultOK, primed)

	// Sentinel passdb — if the chain runs, the test fails. The
	// cache hit must short-circuit before reaching it.
	sentinel := &stubPassdb{
		result: ResultFail,
		setBag: func(f *Fields) {
			t.Errorf("passdb chain consulted despite cache hit")
		},
	}
	srv := NewServer([]Passdb{sentinel}, WithCache(cache))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t80\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	if !strings.HasPrefix(sc.Text(), "OK\t80\tuser=alice") {
		t.Errorf("cache hit not surfaced as OK: %q", sc.Text())
	}
}

// TestServer_Cache_WrongPasswordSkipsCache — a primed positive
// entry with the right password is irrelevant when the wire
// caller sends a different password: cache rejects (mismatch),
// chain runs and rejects, attempted-password path takes
// failure-delay etc.
func TestServer_Cache_WrongPasswordSkipsCache(t *testing.T) {
	cache := NewCache(1<<20, time.Minute, time.Minute)
	cache.Insert(MakeCacheKey("imap", "alice"), "alice", "right", ResultOK, NewFields())

	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "right"}},
		WithCache(cache),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	// Send WRONG password — cache must NOT pass it just because
	// the (key, OK) entry exists; instead the chain runs (and
	// rejects, since credPassdb knows only "right").
	fmt.Fprintf(conn, "AUTH\t81\tPLAIN\tservice=imap\tresp=\x00alice\x00WRONG\n")
	if !sc.Scan() {
		t.Fatalf("no reply: %v", sc.Err())
	}
	if !strings.HasPrefix(sc.Text(), "FAIL\t81") {
		t.Errorf("wrong password got OK from cache: %q", sc.Text())
	}
}

// TestServer_Cache_SeedsOnSuccess — first auth misses, runs the
// chain, then the second identical auth hits the cache (passdb
// is replaced with a sentinel that fails if invoked).
func TestServer_Cache_SeedsOnSuccess(t *testing.T) {
	cache := NewCache(1<<20, time.Minute, time.Minute)
	// First server runs the real passdb so cache gets seeded.
	srv := NewServer(
		[]Passdb{&credPassdb{"alice", "secret"}},
		WithCache(cache),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "AUTH\t90\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "OK\t90") {
		t.Fatalf("first auth: %q (err %v)", sc.Text(), sc.Err())
	}

	// Same conn, second auth — cache should answer without
	// touching the chain. Verify via cache hit-counter.
	hitsBefore, _, _, _ := cache.Stats()
	fmt.Fprintf(conn, "AUTH\t91\tPLAIN\tservice=imap\tresp=\x00alice\x00secret\n")
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "OK\t91") {
		t.Fatalf("second auth: %q", sc.Text())
	}
	hitsAfter, _, _, _ := cache.Stats()
	if hitsAfter <= hitsBefore {
		t.Errorf("second auth did not register a cache hit (before=%d after=%d)", hitsBefore, hitsAfter)
	}
}
