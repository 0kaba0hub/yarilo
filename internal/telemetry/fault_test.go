package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func post(t *testing.T, s *Server, path string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
	return rr.Code
}

// TestGateCheckOpen: an open gate passes immediately.
func TestGateCheckOpen(t *testing.T) {
	g := NewGate()
	if err := g.Check(context.Background()); err != nil {
		t.Fatalf("open gate should pass: %v", err)
	}
}

// TestGateCheckWedgedTimesOut: a wedged gate fails Check on the context deadline
// rather than blocking forever — the property the watchdog relies on.
func TestGateCheckWedgedTimesOut(t *testing.T) {
	g := NewGate()
	if !g.Wedge() {
		t.Fatal("first Wedge should succeed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.Check(ctx); err == nil {
		t.Fatal("a wedged gate must fail Check")
	}
}

// TestGateWedgeIsIdempotentlyReported: wedging an already-wedged gate reports false.
func TestGateWedgeIsIdempotentlyReported(t *testing.T) {
	g := NewGate()
	if !g.Wedge() {
		t.Fatal("first Wedge should take the token")
	}
	if g.Wedge() {
		t.Fatal("second Wedge should report already wedged")
	}
}

// TestFaultEndpointAbsentWithoutOptIn: no Fault gate means the endpoint is not
// registered, so it cannot be reached by accident.
func TestFaultEndpointAbsentWithoutOptIn(t *testing.T) {
	s := NewWithOptions(Options{Addr: ":0"})
	if code := post(t, s, "/debug/fault/deadlock"); code != http.StatusNotFound {
		t.Fatalf("fault endpoint = %d, want 404 when not opted in", code)
	}
}

// TestFaultEndpointRejectsGET: the switch must be a POST, never a stray GET
// (a probe or a crawler) that restarts the pod.
func TestFaultEndpointRejectsGET(t *testing.T) {
	s := NewWithOptions(Options{Addr: ":0", Fault: NewGate()})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/debug/fault/deadlock", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rr.Code)
	}
}

// TestFaultEndpointWedgesThenConflicts: first POST wedges (200), a second
// reports the gate is already wedged (409).
func TestFaultEndpointWedgesThenConflicts(t *testing.T) {
	s := NewWithOptions(Options{Addr: ":0", Fault: NewGate()})
	if code := post(t, s, "/debug/fault/deadlock"); code != http.StatusOK {
		t.Fatalf("first POST = %d, want 200", code)
	}
	if code := post(t, s, "/debug/fault/deadlock"); code != http.StatusConflict {
		t.Fatalf("second POST = %d, want 409", code)
	}
}

// TestFaultInjectionTripsHealthzEndToEnd is the #904 acceptance shape: the same
// gate feeds the watchdog Check and the fault endpoint. Wedging it via the
// endpoint drives the self-check to fail, and after the threshold /healthz fails
// while the process is otherwise fine.
func TestFaultInjectionTripsHealthzEndToEnd(t *testing.T) {
	gate := NewGate()
	s := NewWithOptions(Options{
		Addr:  ":0",
		Fault: gate,
		Watchdog: WatchdogOptions{
			Check:            gate.Check,
			Interval:         time.Second,
			Timeout:          20 * time.Millisecond,
			FailureThreshold: 2,
		},
	})
	if code := hit(t, s, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 before fault", code)
	}
	if code := post(t, s, "/debug/fault/deadlock"); code != http.StatusOK {
		t.Fatalf("fault POST = %d, want 200", code)
	}
	ctx := context.Background()
	s.wd.tick(ctx)
	s.wd.tick(ctx)
	if code := hit(t, s, "/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz = %d, want 503 after fault injection", code)
	}
}
