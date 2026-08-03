package ftsproto_test

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
)

// slowService blocks every Lookup on a barrier, so a test can observe how many
// calls are in flight at once. That is the property the pool exists for: with a
// single connection they arrive one at a time however many callers there are.
type slowService struct {
	inFlight atomic.Int32
	peak     atomic.Int32
	release  chan struct{}
}

func (s *slowService) enter() {
	n := s.inFlight.Add(1)
	for {
		peak := s.peak.Load()
		if n <= peak || s.peak.CompareAndSwap(peak, n) {
			break
		}
	}
}

func (s *slowService) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	s.enter()
	<-s.release
	s.inFlight.Add(-1)
	return fts.Result{}, nil
}

func (s *slowService) Index(string, fts.MailboxRef, uint32, int) error       { return nil }
func (s *slowService) Prepend(string, fts.MailboxRef, uint32) error          { return nil }
func (s *slowService) Expunge(string, fts.MailboxRef, uint32) error          { return nil }
func (s *slowService) Status(string, fts.MailboxRef) (uint32, uint32, error) { return 0, 0, nil }
func (s *slowService) Rescan(string, fts.MailboxRef) error                   { return nil }
func (s *slowService) Optimize(string) error                                 { return nil }

func serveSlow(t *testing.T) (addr string, svc *slowService) {
	t.Helper()
	svc = &slowService{release: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go ftsproto.Serve(ln, svc)       //nolint:errcheck
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	return ln.Addr().String(), svc
}

// The point of the pool: several lookups reach the service at once. A single
// connection serialises them, so this is what tells a real pool from a limiter
// placed over a queue.
func TestPoolRunsLookupsConcurrently(t *testing.T) {
	addr, svc := serveSlow(t)
	const size = 4
	p := ftsproto.NewPool(addr, size, 2*time.Second)
	t.Cleanup(func() { p.Close() }) //nolint:errcheck

	var wg sync.WaitGroup
	for i := 0; i < size; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Lookup("u1", fts.MailboxRef{Name: "INBOX"}, fts.Query{}) //nolint:errcheck
		}()
	}

	// Wait for the service to see all of them before releasing any.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && svc.peak.Load() < int32(size) {
		time.Sleep(5 * time.Millisecond)
	}
	peak := svc.peak.Load()
	close(svc.release)
	wg.Wait()

	if peak < int32(size) {
		t.Errorf("peak concurrency = %d, want %d — the pool is not producing parallelism", peak, size)
	}
}

// A pool of one is exactly today's behaviour, which is what makes the default
// safe to change: nothing about the single-connection path is altered.
func TestPoolOfOneSerialises(t *testing.T) {
	addr, svc := serveSlow(t)
	p := ftsproto.NewPool(addr, 1, 2*time.Second)
	t.Cleanup(func() { p.Close() }) //nolint:errcheck

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Lookup("u1", fts.MailboxRef{Name: "INBOX"}, fts.Query{}) //nolint:errcheck
		}()
	}
	time.Sleep(150 * time.Millisecond)
	peak := svc.peak.Load()
	close(svc.release)
	wg.Wait()

	if peak > 1 {
		t.Errorf("peak concurrency = %d with one connection, want 1", peak)
	}
}

// A caller that cannot get a connection is told so, and told it distinctly:
// "busy" and "failed" call for different reactions.
func TestPoolExhaustionIsItsOwnError(t *testing.T) {
	addr, svc := serveSlow(t)
	p := ftsproto.NewPool(addr, 1, 100*time.Millisecond)
	t.Cleanup(func() { p.Close() }) //nolint:errcheck

	busy := make(chan struct{})
	go func() {
		close(busy)
		p.Lookup("u1", fts.MailboxRef{Name: "INBOX"}, fts.Query{}) //nolint:errcheck
	}()
	<-busy
	for svc.inFlight.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	_, err := p.Lookup("u2", fts.MailboxRef{Name: "INBOX"}, fts.Query{})
	close(svc.release)
	if !errors.Is(err, ftsproto.ErrPoolExhausted) {
		t.Errorf("err = %v, want ErrPoolExhausted", err)
	}
}

// Size below one is treated as one rather than deadlocking on an empty pool.
func TestPoolSizeFloor(t *testing.T) {
	for _, size := range []int{-1, 0, 1} {
		if got := ftsproto.NewPool("127.0.0.1:1", size, time.Second).Size(); got != 1 {
			t.Errorf("size %d gave a pool of %d, want 1", size, got)
		}
	}
}
