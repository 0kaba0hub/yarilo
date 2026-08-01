package telemetry

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// LivenessCheck is a component's cheap, local self-check, run on a timer by the
// watchdog to prove the request path is not wedged (a deadlock, a hung NFS
// handle) while the accept loop and this HTTP server keep answering.
//
// It MUST NOT touch a shared dependency: a database or Redis hiccup would then
// trip every pod's watchdog at once and restart the whole tier, which is
// strictly worse than the degraded state it replaces. Probe a local lock, stat
// the mail store, resolve through the in-process cache — never the backend
// everyone shares. The watchdog bounds each run with its own timeout, so a
// check that blocks forever counts as a failure rather than stalling the loop.
type LivenessCheck func(context.Context) error

// WatchdogOptions configures the timer-driven liveness watchdog. It is opt-in:
// a component supplies a Check only once it has a self-check worth restarting on.
// With no Check the watchdog never runs and /healthz stays unconditional — the
// correct answer for a component whose dead accept loop already os.Exit(1)s.
type WatchdogOptions struct {
	// Check is the self-check. Nil disables the watchdog entirely.
	Check LivenessCheck
	// Interval is the gap between checks. It is timer-driven on purpose: "traffic
	// arrived recently" is not a health signal, because warden and the login pods
	// legitimately sit idle, and a traffic-driven liveness would restart them at
	// night.
	Interval time.Duration
	// Timeout bounds a single check and MUST be shorter than Interval; otherwise a
	// hung check silently stops reporting and produces the false restart the
	// watchdog exists to avoid. A check that exceeds it is counted as failed.
	Timeout time.Duration
	// FailureThreshold is how many CONSECUTIVE failed checks trip /healthz. A
	// single slow NFS write must not kill a busy pod; the probe's own
	// failureThreshold stacks on top of this.
	FailureThreshold int
}

// watchdog runs the self-check on a timer and records whether /healthz should
// currently fail.
type watchdog struct {
	check     LivenessCheck
	interval  time.Duration
	timeout   time.Duration
	threshold int

	fails   atomic.Int64
	tripped atomic.Bool
}

// newWatchdog returns a watchdog for opts, or nil when no Check is set. Defaults
// fill in any non-positive tunable so a partially-configured opt-in is still safe.
func newWatchdog(opts WatchdogOptions) *watchdog {
	if opts.Check == nil {
		return nil
	}
	w := &watchdog{
		check:     opts.Check,
		interval:  opts.Interval,
		timeout:   opts.Timeout,
		threshold: opts.FailureThreshold,
	}
	if w.interval <= 0 {
		w.interval = 10 * time.Second
	}
	// The timeout must stay strictly below the interval, or a hung check would
	// overrun the next tick and never let the timestamp advance.
	if w.timeout <= 0 || w.timeout >= w.interval {
		w.timeout = w.interval / 2
	}
	if w.threshold <= 0 {
		w.threshold = 3
	}
	return w
}

// run ticks until ctx is done. Each tick runs the check under its own timeout in
// a separate goroutine so a check that ignores cancellation cannot stall the
// loop — a timeout is observed as a failure and the leaked goroutine is
// irrelevant on the path to a restart.
func (w *watchdog) run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *watchdog) tick(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.check(cctx) }()

	var err error
	select {
	case err = <-done:
	case <-cctx.Done():
		err = cctx.Err()
	}

	if err == nil {
		w.fails.Store(0)
		if w.tripped.Swap(false) {
			slog.Info("telemetry: liveness watchdog recovered")
		}
		return
	}
	n := w.fails.Add(1)
	if int(n) >= w.threshold && !w.tripped.Swap(true) {
		slog.Error("telemetry: liveness watchdog tripped — /healthz will fail so the container restarts",
			"consecutive_failures", n, "err", err)
	}
}

// unhealthy reports whether the watchdog has tripped and /healthz should fail.
func (w *watchdog) unhealthy() bool { return w != nil && w.tripped.Load() }
