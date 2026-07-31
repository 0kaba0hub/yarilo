package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
)

// Gate is a context-aware mutex a liveness self-check enters to prove the
// request path is not blocked. Under normal operation the token is always free,
// so Check returns immediately; the fault-injection endpoint (#904) can Wedge it
// to hold the token forever, which drives the self-check — and therefore the
// watchdog — into the tripped state on a live pod without a real deadlock.
//
// It is a channel of one token rather than a sync.Mutex because acquisition must
// be bounded by a context: a truly wedged gate must fail the check on the
// watchdog's timeout, not block its goroutine indefinitely.
type Gate struct {
	sem chan struct{}
}

// NewGate returns an open gate.
func NewGate() *Gate {
	g := &Gate{sem: make(chan struct{}, 1)}
	g.sem <- struct{}{}
	return g
}

// Check acquires and immediately releases the token, bounded by ctx. It returns
// nil when the gate is open and ctx.Err() (wrapped) when the gate is wedged and
// the deadline passes — the signal the watchdog counts as a failure.
func (g *Gate) Check(ctx context.Context) error {
	select {
	case <-g.sem:
		g.sem <- struct{}{}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("telemetry: liveness gate wedged: %w", ctx.Err())
	}
}

// Wedge takes the token and never returns it, simulating a permanently held
// lock. It reports false if the gate was already wedged. This is reachable only
// through the fault-injection endpoint, which is off unless explicitly enabled.
func (g *Gate) Wedge() bool {
	select {
	case <-g.sem:
		return true
	default:
		return false
	}
}

// faultHandler wedges the gate on POST, so an operator can confirm on a live pod
// that a blocked request path trips /healthz while /readyz and the accept loop
// stay healthy (#904 acceptance). It is registered only when fault injection is
// enabled in config, and is a no-op safeguard otherwise.
func (s *Server) faultHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.fault == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !s.fault.Wedge() {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte("already wedged\n")) //nolint:errcheck
		return
	}
	slog.Warn("telemetry: fault injected — liveness gate wedged; the watchdog will trip and the container will restart")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("wedged\n")) //nolint:errcheck
}
