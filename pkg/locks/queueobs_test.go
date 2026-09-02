package locks

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func histLabelled(t *testing.T, v *prometheus.HistogramVec, labels ...string) (float64, uint64) {
	t.Helper()
	h, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get %v: %v", labels, err)
	}
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %v: %v", labels, err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

func counterLabelled(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %v: %v", labels, err)
	}
	return m.GetCounter().GetValue()
}

// busyLocker refuses the first busyFor attempts and then succeeds, which is a
// contender losing that many draws in a row.
type busyLocker struct {
	busyFor int
	calls   int
}

func (b *busyLocker) Lock(context.Context, string, string, time.Duration) (Lock, error) {
	b.calls++
	if b.calls <= b.busyFor {
		return Lock{}, ErrBusy
	}
	return Lock{ID: "l"}, nil
}
func (b *busyLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (Lock, error) {
	return b.Lock(ctx, r, o, ttl)
}
func (b *busyLocker) Unlock(context.Context, string) error { return nil }
func (b *busyLocker) Renew(context.Context, string, time.Duration) error {
	return nil
}
func (b *busyLocker) HoldsResource(string) bool                               { return false }
func (b *busyLocker) Close() error                                            { return nil }
func (b *busyLocker) Subscribe(context.Context, string) (<-chan Event, error) { return nil, nil }
func (b *busyLocker) Emit(context.Context, string, EventType, string) error   { return nil }
func (b *busyLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

// One acquisition is one observation, whatever it cost. A contender that lost
// four draws is one slow acquisition, and the wait recorded is the whole wait
// rather than the last sleep (#1640).
func TestAWaitIsMeasuredOncePerAcquisitionNotPerAttempt(t *testing.T) {
	const key = "mbox:u@x/INBOX"
	_, countBefore := histLabelled(t, clientAcquireWait, "mbox")
	sumBefore, _ := histLabelled(t, clientAcquireWait, "mbox")
	attemptSum, attemptCount := histLabelled(t, clientAcquireAttempts, "mbox")

	l := &busyLocker{busyFor: 4}
	if _, err := Acquire(context.Background(), l, key, "owner", time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	sum, count := histLabelled(t, clientAcquireWait, "mbox")
	if count-countBefore != 1 {
		t.Errorf("the wait was observed %d times for one acquisition, want 1", count-countBefore)
	}
	// Four backoffs of at least 0.75ms each: the wait is the whole wait, and a
	// per-attempt reading would divide it.
	if waited := sum - sumBefore; waited < 0.003 {
		t.Errorf("the acquisition recorded %.4fs of waiting after losing four draws, which is less "+
			"than the backoffs it slept: the clock is not spanning the retries", waited)
	}
	newSum, newCount := histLabelled(t, clientAcquireAttempts, "mbox")
	if newCount-attemptCount != 1 || newSum-attemptSum != 5 {
		t.Errorf("attempts recorded %v observations summing %v, want 1 observation of 5",
			newCount-attemptCount, newSum-attemptSum)
	}
}

// A contender that runs out of time is counted where it happened, with how many
// attempts it had burned. That count is the stall, in the raw.
func TestAContenderThatRunsOutOfTimeIsCountedWithItsAttempts(t *testing.T) {
	const key = "mbox:u@x/INBOX"
	before := counterLabelled(t, clientGaveUp, "mbox", "2-3")

	l := &busyLocker{busyFor: 1 << 30}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, l, key, "owner", time.Second); err == nil {
		t.Fatal("the acquisition succeeded against a locker that never yields")
	}
	if got := counterLabelled(t, clientGaveUp, "mbox", "2-3") - before; got != 1 {
		t.Errorf("gave-up count in the 2-3 bucket moved by %v, want 1 -- a 4ms deadline against "+
			"1ms and 2ms backoffs gives up on the third attempt", got)
	}
}

// Attempts become a bucket, not a number: the label is written by whoever
// stalls, and one series per attempt count is one series per contender.
func TestAttemptsAreBucketed(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{1, "1"}, {2, "2-3"}, {3, "2-3"}, {4, "4-7"}, {15, "8-15"}, {16, "16-31"}, {64, "64+"}, {5000, "64+"}} {
		if got := attemptBucket(tc.n); got != tc.want {
			t.Errorf("attemptBucket(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
