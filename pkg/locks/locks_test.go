package locks_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// lockerSuite is the contract test suite. Every backend must pass it.
// Spin up a (server, client) pair via the factory and run.
type lockerFactory func(t *testing.T) (locker locks.Locker, cleanup func())

func runSuite(t *testing.T, name string, factory lockerFactory) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Run("AcquireRelease", func(t *testing.T) { testAcquireRelease(t, factory) })
		t.Run("Contention", func(t *testing.T) { testContention(t, factory) })
		t.Run("Renew", func(t *testing.T) { testRenew(t, factory) })
		t.Run("UnlockUnknown", func(t *testing.T) { testUnlockUnknown(t, factory) })
		t.Run("Subscribe", func(t *testing.T) { testSubscribe(t, factory) })
		t.Run("ResourceKeyOrdering", func(t *testing.T) { testResourceKeys(t, factory) })
	})
}

func testAcquireRelease(t *testing.T, factory lockerFactory) {
	l, cleanup := factory(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := l.Lock(ctx, "mbox:alice:INBOX", "owner-1", 5*time.Second)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if lock.ID == "" {
		t.Fatal("expected non-empty lock id")
	}
	if err := l.Unlock(ctx, lock.ID); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// After release, the resource is free again.
	lock2, err := l.Lock(ctx, "mbox:alice:INBOX", "owner-2", 5*time.Second)
	if err != nil {
		t.Fatalf("second lock: %v", err)
	}
	_ = l.Unlock(ctx, lock2.ID)
}

func testContention(t *testing.T, factory lockerFactory) {
	l, cleanup := factory(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := l.Lock(ctx, "mbox:bob:INBOX", "owner-A", 5*time.Second)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer func() { _ = l.Unlock(context.Background(), a.ID) }()
	_, err = l.Lock(ctx, "mbox:bob:INBOX", "owner-B", 5*time.Second)
	if !errors.Is(err, locks.ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
}

func testRenew(t *testing.T, factory lockerFactory) {
	l, cleanup := factory(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := l.Lock(ctx, "idx:alice", "owner", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := l.Renew(ctx, lock.ID, time.Second); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := l.Unlock(ctx, lock.ID); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// Renew after release surfaces ErrExpired (Redis script) or ErrExpired (memory).
	if err := l.Renew(ctx, lock.ID, time.Second); !errors.Is(err, locks.ErrExpired) {
		t.Fatalf("expected ErrExpired after release, got %v", err)
	}
}

func testUnlockUnknown(t *testing.T, factory lockerFactory) {
	l, cleanup := factory(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := l.Unlock(ctx, "bogus-lock-id-0000000000000000")
	if !errors.Is(err, locks.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func testSubscribe(t *testing.T, factory lockerFactory) {
	l, cleanup := factory(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := l.Subscribe(ctx, "mbox:carol:INBOX")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Give the server a moment to register the subscription before emitting.
	time.Sleep(100 * time.Millisecond)
	if err := l.Emit(ctx, "mbox:carol:INBOX", locks.EventDelivered, "msg-1"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	select {
	case evt := <-ch:
		if evt.Type != locks.EventDelivered || evt.Payload != "msg-1" {
			t.Fatalf("unexpected event %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func testResourceKeys(t *testing.T, factory lockerFactory) {
	l, cleanup := factory(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	owner := "deadlock-test"
	idx, err := l.Lock(ctx, locks.IndexKey("dave"), owner, 5*time.Second)
	if err != nil {
		t.Fatalf("idx lock: %v", err)
	}
	defer func() { _ = l.Unlock(context.Background(), idx.ID) }()
	mb, err := l.Lock(ctx, locks.MailboxKey("dave", "INBOX"), owner, 5*time.Second)
	if err != nil {
		t.Fatalf("mbox lock: %v", err)
	}
	defer func() { _ = l.Unlock(context.Background(), mb.ID) }()
	dl, err := l.Lock(ctx, locks.DeliverKey("dave", "INBOX"), owner, 5*time.Second)
	if err != nil {
		t.Fatalf("deliver lock: %v", err)
	}
	_ = l.Unlock(ctx, dl.ID)
}

// --- factories ------------------------------------------------------------

func memoryFactory(t *testing.T) (locks.Locker, func()) {
	t.Helper()
	socket := shortSocketPath(t)
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(10 * time.Millisecond))
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), newTestMetrics(t, "embedded"))
	ln, err := locks.ListenUnix(socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, ln)
		close(done)
	}()
	// Wait until socket accepts.
	if !waitDial(t, "unix", socket) {
		t.Fatal("server did not start")
	}
	client, err := locks.NewClient(context.Background(), locks.DialUnix(socket))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = client.Close()
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	}
	return client, cleanup
}

func redisFactory(t *testing.T) (locks.Locker, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	backend := locks.NewRedisBackend(rdb)
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), newTestMetrics(t, "remote"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, ln)
		close(done)
	}()
	if !waitDial(t, "tcp", addr) {
		t.Fatal("server did not start")
	}
	client, err := locks.NewClient(context.Background(), func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = client.Close()
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	}
	return client, cleanup
}

func TestSuites(t *testing.T) {
	runSuite(t, "embedded", memoryFactory)
	runSuite(t, "remote", redisFactory)
}

// --- WithLock helper test (locker-implementation independent) -------------

func TestWithLock(t *testing.T) {
	l, cleanup := memoryFactory(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var called bool
	err := locks.WithLock(ctx, l, "mbox:test:INBOX", "test-owner", 500*time.Millisecond, 100*time.Millisecond, func(ctx context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !called {
		t.Fatal("fn not called")
	}
	// Lock should be released after WithLock returns.
	lock, err := l.Lock(ctx, "mbox:test:INBOX", "second-owner", time.Second)
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	_ = l.Unlock(ctx, lock.ID)
}

func TestWithLockInvalidRenewEvery(t *testing.T) {
	l, cleanup := memoryFactory(t)
	defer cleanup()
	err := locks.WithLock(context.Background(), l, "r", "o", time.Second, 2*time.Second, func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("expected error for renewEvery >= ttl")
	}
}

// --- helpers --------------------------------------------------------------

// shortSocketPath returns a Unix-socket path short enough to satisfy
// sockaddr_un.sun_path on macOS/BSD (104 bytes). The standard t.TempDir()
// under /var/folders on macOS is too long. Linux's 108-byte limit is rarely
// hit so t.TempDir() works there, but for portability we use a short path
// everywhere.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "l.sock")
}

func waitDial(t *testing.T, network, addr string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout(network, addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// newTestMetrics avoids polluting prometheus.DefaultRegisterer across tests.
func newTestMetrics(t *testing.T, mode string) *locks.Metrics {
	t.Helper()
	r := prometheus.NewRegistry()
	return locks.NewMetrics(r, mode)
}

// Compile-time guard: ensure Client satisfies Locker.
var _ locks.Locker = (*locks.Client)(nil)

// Compile-time guard: ensure MemoryBackend and RedisBackend satisfy Backend.
var _ locks.Backend = (*locks.MemoryBackend)(nil)
var _ locks.Backend = (*locks.RedisBackend)(nil)

// ConcurrencyStress exercises N goroutines fighting over the same resource.
// Confirms only one holder at a time and no leaks.
func TestConcurrentAcquire(t *testing.T) {
	l, cleanup := memoryFactory(t)
	defer cleanup()
	const workers = 8
	const iterations = 20
	var (
		wg      sync.WaitGroup
		held    int32
		maxHeld int32
		mu      sync.Mutex
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				for {
					lock, err := l.Lock(ctx, "mbox:hot:INBOX", "w", 200*time.Millisecond)
					if err == nil {
						mu.Lock()
						held++
						if held > maxHeld {
							maxHeld = held
						}
						mu.Unlock()
						time.Sleep(5 * time.Millisecond)
						mu.Lock()
						held--
						mu.Unlock()
						_ = l.Unlock(ctx, lock.ID)
						break
					}
					if !errors.Is(err, locks.ErrBusy) {
						t.Errorf("worker %d: lock: %v", id, err)
						break
					}
					time.Sleep(time.Millisecond)
				}
				cancel()
			}
		}(i)
	}
	wg.Wait()
	if maxHeld != 1 {
		t.Fatalf("mutual exclusion violated: maxHeld=%d", maxHeld)
	}
}
