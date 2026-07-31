package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hit runs one request against the server's mux and returns the status code.
func hit(t *testing.T, s *Server, path string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr.Code
}

// TestHealthzUnconditionalWithoutWatchdog pins the back-compat contract: a
// component that wires no Check keeps a /healthz that only fails when the
// handler itself cannot run.
func TestHealthzUnconditionalWithoutWatchdog(t *testing.T) {
	s := NewWithOptions(Options{Addr: ":0"})
	if s.wd != nil {
		t.Fatal("no Check should mean no watchdog")
	}
	if code := hit(t, s, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", code)
	}
}

// TestWatchdogTripsOnlyAfterConsecutiveFailures is the core guard-rail: a single
// failure must not fail /healthz — only FailureThreshold in a row may.
func TestWatchdogTripsOnlyAfterConsecutiveFailures(t *testing.T) {
	failing := func(context.Context) error { return errors.New("wedged") }
	s := NewWithOptions(Options{Addr: ":0", Watchdog: WatchdogOptions{
		Check: failing, Interval: time.Second, Timeout: 100 * time.Millisecond, FailureThreshold: 3,
	}})
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		s.wd.tick(ctx)
		if code := hit(t, s, "/healthz"); code != http.StatusOK {
			t.Fatalf("after %d/3 failures /healthz = %d, want still 200", i, code)
		}
	}
	s.wd.tick(ctx) // third consecutive failure
	if code := hit(t, s, "/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("after 3/3 failures /healthz = %d, want 503", code)
	}
}

// TestWatchdogResetsOnSuccess: a success anywhere in the streak clears the
// counter, so an intermittent slow check never trips the threshold.
func TestWatchdogResetsOnSuccess(t *testing.T) {
	var fail bool
	check := func(context.Context) error {
		if fail {
			return errors.New("slow")
		}
		return nil
	}
	s := NewWithOptions(Options{Addr: ":0", Watchdog: WatchdogOptions{
		Check: check, Interval: time.Second, Timeout: 100 * time.Millisecond, FailureThreshold: 3,
	}})
	ctx := context.Background()

	fail = true
	s.wd.tick(ctx)
	s.wd.tick(ctx) // 2 failures
	fail = false
	s.wd.tick(ctx) // success resets
	fail = true
	s.wd.tick(ctx)
	s.wd.tick(ctx) // only 2 in a row again
	if code := hit(t, s, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 — a reset streak must not trip", code)
	}
}

// TestWatchdogRecovers: once tripped, a passing check clears /healthz back to
// 200 so a recovered pod is not restarted forever.
func TestWatchdogRecovers(t *testing.T) {
	var fail = true
	check := func(context.Context) error {
		if fail {
			return errors.New("wedged")
		}
		return nil
	}
	s := NewWithOptions(Options{Addr: ":0", Watchdog: WatchdogOptions{
		Check: check, Interval: time.Second, Timeout: 100 * time.Millisecond, FailureThreshold: 2,
	}})
	ctx := context.Background()

	s.wd.tick(ctx)
	s.wd.tick(ctx)
	if code := hit(t, s, "/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz = %d, want 503 after tripping", code)
	}
	fail = false
	s.wd.tick(ctx)
	if code := hit(t, s, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 after recovery", code)
	}
}

// TestWatchdogCountsAHungCheckAsFailure is the property that makes the watchdog
// worth having: a self-check that ignores cancellation and blocks forever — the
// deadlock shape — must be observed as a failure via the timeout, not stall the
// loop and silently keep /healthz green.
func TestWatchdogCountsAHungCheckAsFailure(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	hung := func(context.Context) error { <-block; return nil }
	s := NewWithOptions(Options{Addr: ":0", Watchdog: WatchdogOptions{
		Check: hung, Interval: time.Second, Timeout: 20 * time.Millisecond, FailureThreshold: 2,
	}})
	ctx := context.Background()

	s.wd.tick(ctx)
	s.wd.tick(ctx)
	if code := hit(t, s, "/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz = %d, want 503 — a hung check must count as a failure", code)
	}
}

// TestNewWatchdogDefaults: a partially-configured opt-in still yields a safe
// watchdog with a timeout strictly below the interval.
func TestNewWatchdogDefaults(t *testing.T) {
	w := newWatchdog(WatchdogOptions{Check: func(context.Context) error { return nil }})
	if w == nil {
		t.Fatal("a Check must produce a watchdog")
	}
	if w.timeout >= w.interval {
		t.Fatalf("timeout %v must be < interval %v", w.timeout, w.interval)
	}
	if w.threshold <= 0 {
		t.Fatalf("threshold defaulted to %d", w.threshold)
	}
}

// TestNewWatchdogNilWithoutCheck: no Check, no watchdog.
func TestNewWatchdogNilWithoutCheck(t *testing.T) {
	if w := newWatchdog(WatchdogOptions{Interval: time.Second}); w != nil {
		t.Fatal("no Check must yield a nil watchdog")
	}
}

// TestWatchdogRunTicks exercises the timer loop end to end: a short interval
// with an always-failing check trips /healthz, and cancelling ctx stops it.
func TestWatchdogRunTicks(t *testing.T) {
	s := NewWithOptions(Options{Addr: ":0", Watchdog: WatchdogOptions{
		Check:    func(context.Context) error { return errors.New("x") },
		Interval: 10 * time.Millisecond, Timeout: 5 * time.Millisecond, FailureThreshold: 2,
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.wd.run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		if hit(t, s, "/healthz") == http.StatusServiceUnavailable {
			return
		}
		select {
		case <-deadline:
			t.Fatal("watchdog run loop never tripped /healthz")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
