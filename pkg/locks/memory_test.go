package locks

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryBackendTTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := &fakeClock{t: now}
	b := NewMemoryBackend(WithSweepInterval(10*time.Millisecond), WithNow(clock.Now))
	defer b.Close()
	ctx := context.Background()

	id, _, err := b.Acquire(ctx, "r1", "o1", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Advance past the TTL and let the sweeper observe.
	clock.advance(2 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if err := b.Renew(ctx, id, time.Second); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired after sweep, got %v", err)
	}
	// Resource is free; a new owner can acquire.
	_, _, err = b.Acquire(ctx, "r1", "o2", time.Second)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
}

func TestMemoryBackendBusyReturnsCurrentOwner(t *testing.T) {
	b := NewMemoryBackend()
	defer b.Close()
	ctx := context.Background()
	_, _, err := b.Acquire(ctx, "r", "alice", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, current, err := b.Acquire(ctx, "r", "bob", time.Minute)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	if current != "alice" {
		t.Fatalf("expected current owner alice, got %q", current)
	}
}

func TestMemoryBackendReleaseNotFound(t *testing.T) {
	b := NewMemoryBackend()
	defer b.Close()
	if err := b.Release(context.Background(), "bogus"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryBackendRejectsEmptyArgs(t *testing.T) {
	b := NewMemoryBackend()
	defer b.Close()
	if _, _, err := b.Acquire(context.Background(), "", "owner", time.Second); err == nil {
		t.Fatal("expected error for empty resource")
	}
	if _, _, err := b.Acquire(context.Background(), "r", "", time.Second); err == nil {
		t.Fatal("expected error for empty owner")
	}
}

func TestMemoryBackendPublishSubscribe(t *testing.T) {
	b := NewMemoryBackend()
	defer b.Close()
	ctx := context.Background()
	ch, cancel, err := b.Subscribe(ctx, "r")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()
	if err := b.Publish(ctx, "r", EventDelivered, "p1"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case evt := <-ch:
		if evt.Type != EventDelivered || evt.Payload != "p1" {
			t.Fatalf("unexpected event %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestMemoryBackendSubscribeDropsSlowReceiver(t *testing.T) {
	// Slow consumer must not block the publisher; non-blocking send semantics.
	b := NewMemoryBackend()
	defer b.Close()
	ctx := context.Background()
	_, cancel, err := b.Subscribe(ctx, "r")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()
	// Don't read from the channel — fill it up with many publishes.
	for i := 0; i < 1000; i++ {
		if err := b.Publish(ctx, "r", EventChanged, ""); err != nil {
			t.Fatalf("publish #%d: %v", i, err)
		}
	}
	// We expect no deadlock and no panic; assertion is just reaching this line.
}

// fakeClock provides a deterministic time source for TTL tests.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
