package lmtp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// fakeLocker is a minimal locks.Locker stand-in that implements
// just IncrementCounter — the only method checkRecipientRate
// touches. Every other method panics so accidental drift is
// caught immediately.
type fakeLocker struct {
	mu       sync.Mutex
	counters map[string]int64
	err      error // set non-nil to simulate counter outage
}

func newFakeLocker() *fakeLocker {
	return &fakeLocker{counters: make(map[string]int64)}
}

func (f *fakeLocker) IncrementCounter(_ context.Context, key string, delta int64) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counters[key] += delta
	return f.counters[key], nil
}

func (f *fakeLocker) Lock(context.Context, string, string, time.Duration) (locks.Lock, error) {
	panic("not implemented")
}
func (f *fakeLocker) LockShared(context.Context, string, string, time.Duration) (locks.Lock, error) {
	panic("not implemented")
}
func (f *fakeLocker) Unlock(context.Context, string) error               { panic("not implemented") }
func (f *fakeLocker) Renew(context.Context, string, time.Duration) error { panic("not implemented") }
func (f *fakeLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	panic("not implemented")
}
func (f *fakeLocker) Emit(context.Context, string, locks.EventType, string) error {
	panic("not implemented")
}
func (f *fakeLocker) HoldsResource(string) bool { panic("not implemented") }
func (f *fakeLocker) Close() error              { return nil }

func TestCheckRecipientRate_AllowsWithinBurst(t *testing.T) {
	l := newFakeLocker()
	ctx := context.Background()
	const burst = 5
	for i := 1; i <= burst; i++ {
		if err := checkRecipientRate(ctx, l, "10.0.0.1", "alice@x", burst, 60); err != nil {
			t.Errorf("call %d (within burst): %v", i, err)
		}
	}
}

func TestCheckRecipientRate_DeniesPastBurst(t *testing.T) {
	l := newFakeLocker()
	ctx := context.Background()
	const burst = 3
	for i := 1; i <= burst; i++ {
		if err := checkRecipientRate(ctx, l, "10.0.0.1", "alice@x", burst, 60); err != nil {
			t.Fatalf("call %d (within burst): %v", i, err)
		}
	}
	if err := checkRecipientRate(ctx, l, "10.0.0.1", "alice@x", burst, 60); !errors.Is(err, ErrRateLimited) {
		t.Errorf("post-burst call: got %v, want ErrRateLimited", err)
	}
}

func TestCheckRecipientRate_IsolatesKeys(t *testing.T) {
	l := newFakeLocker()
	ctx := context.Background()
	const burst = 1
	_ = checkRecipientRate(ctx, l, "10.0.0.1", "alice@x", burst, 60)
	if err := checkRecipientRate(ctx, l, "10.0.0.1", "alice@x", burst, 60); !errors.Is(err, ErrRateLimited) {
		t.Errorf("first IP retry: want denied, got %v", err)
	}
	if err := checkRecipientRate(ctx, l, "10.0.0.2", "alice@x", burst, 60); err != nil {
		t.Errorf("second IP first call: %v", err)
	}
	if err := checkRecipientRate(ctx, l, "10.0.0.1", "bob@x", burst, 60); err != nil {
		t.Errorf("first IP, different rcpt: %v", err)
	}
}

func TestCheckRecipientRate_NilLockerAlwaysAllows(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if err := checkRecipientRate(context.Background(), nil, "10.0.0.1", "alice@x", 1, 60); err != nil {
			t.Fatalf("call %d with nil locker: %v", i, err)
		}
	}
}

func TestCheckRecipientRate_DisabledByZeroBurstOrWindow(t *testing.T) {
	l := newFakeLocker()
	if err := checkRecipientRate(context.Background(), l, "10.0.0.1", "alice@x", 0, 60); err != nil {
		t.Errorf("burst=0 should disable the check, got %v", err)
	}
	if err := checkRecipientRate(context.Background(), l, "10.0.0.1", "alice@x", 5, 0); err != nil {
		t.Errorf("window=0 should disable the check, got %v", err)
	}
}

func TestCheckRecipientRate_CounterErrorIsNotRateLimit(t *testing.T) {
	l := newFakeLocker()
	l.err = errors.New("simulated outage")
	err := checkRecipientRate(context.Background(), l, "10.0.0.1", "alice@x", 5, 60)
	if err == nil {
		t.Fatal("expected counter outage to surface as an error")
	}
	if errors.Is(err, ErrRateLimited) {
		t.Error("counter outage must not surface as ErrRateLimited — caller decides to accept")
	}
}
