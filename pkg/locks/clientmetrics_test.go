package locks

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// busyThenFree refuses the first n attempts as ErrBusy, then grants.
type busyThenFree struct{ refusals int }

func (b *busyThenFree) Lock(context.Context, string, string, time.Duration) (Lock, error) {
	if b.refusals > 0 {
		b.refusals--
		return Lock{}, ErrBusy
	}
	return Lock{ID: "granted"}, nil
}
func (b *busyThenFree) LockShared(ctx context.Context, r, o string, ttl time.Duration) (Lock, error) {
	return b.Lock(ctx, r, o, ttl)
}
func (b *busyThenFree) Unlock(context.Context, string) error               { return nil }
func (b *busyThenFree) Renew(context.Context, string, time.Duration) error { return nil }
func (b *busyThenFree) Subscribe(context.Context, string) (<-chan Event, error) {
	return nil, nil
}
func (b *busyThenFree) Emit(context.Context, string, EventType, string) error { return nil }
func (b *busyThenFree) HoldsResource(string) bool                             { return false }
func (b *busyThenFree) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}
func (b *busyThenFree) Close() error { return nil }

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// Contention is counted where it is paid.
//
// The lock service counts refusals for the whole deployment, which says how
// often somebody was refused and nothing about who waited or for what. This
// counter sits in the caller, next to the acquisition latency the sleeping
// inflates (#1533).
//
// Two refusals, one grant: three calls to the service, two sleeps. Counting the
// grant as well would make the counter a call count, which the acquisition
// histogram already is.
func TestBusyRetriesAreCountedInTheCaller(t *testing.T) {
	before := counterValue(t, clientBusyRetries)

	l := &busyThenFree{refusals: 2}
	if _, err := Acquire(context.Background(), l, "res", "owner", time.Second); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if got := counterValue(t, clientBusyRetries) - before; got != 2 {
		t.Errorf("counted %v busy retries for two refusals and one grant, want 2", got)
	}
}

// An uncontended acquisition counts nothing, so the counter cannot be mistaken
// for the number of acquisitions -- which is the confusion this whole issue was.
func TestAnUncontendedAcquisitionCountsNoRetry(t *testing.T) {
	before := counterValue(t, clientBusyRetries)

	if _, err := Acquire(context.Background(), &busyThenFree{}, "res", "owner", time.Second); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if got := counterValue(t, clientBusyRetries) - before; got != 0 {
		t.Errorf("counted %v retries for an acquisition nobody contended", got)
	}
}
