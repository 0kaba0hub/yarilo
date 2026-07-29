package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func readyzBody(t *testing.T, s *Server) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, out
}

// TestReadyzWithoutChecksIsReady covers the deliberate default: a component with
// no external dependency is ready once its process is up. This must not depend on
// SetReady, or a component that never calls it would be stuck not-ready forever —
// which is exactly what would have happened to eleven binaries during
// unification had the flag been the fallback.
func TestReadyzWithoutChecksIsReady(t *testing.T) {
	s := NewWithOptions(Options{Addr: ":0"})
	code, body := readyzBody(t, s)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["ready"] != true {
		t.Fatalf("ready = %v, want true", body["ready"])
	}
}

// TestReadyzLifecycleGate covers the opposite case: a component that does have a
// startup phase opts in and stays not-ready until it says otherwise.
func TestReadyzLifecycleGate(t *testing.T) {
	s := NewWithOptions(Options{Addr: ":0", Lifecycle: true})

	if code, _ := readyzBody(t, s); code != http.StatusServiceUnavailable {
		t.Fatalf("before SetReady: status = %d, want 503", code)
	}
	s.SetReady(true)
	if code, _ := readyzBody(t, s); code != http.StatusOK {
		t.Fatalf("after SetReady(true): status = %d, want 200", code)
	}
	s.SetReady(false)
	if code, _ := readyzBody(t, s); code != http.StatusServiceUnavailable {
		t.Fatalf("after SetReady(false): status = %d, want 503", code)
	}
}

// TestReadyzNamesTheFailingCheck is the reason the body is JSON: an operator
// reading a failing probe must learn WHICH dependency is missing.
func TestReadyzNamesTheFailingCheck(t *testing.T) {
	s := NewWithOptions(Options{
		Addr: ":0",
		Checks: []Check{
			{Name: "healthy", Probe: func(context.Context) error { return nil }},
			{Name: "broken", Probe: func(context.Context) error { return errors.New("connection refused") }},
		},
	})

	code, body := readyzBody(t, s)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if body["ready"] != false {
		t.Fatalf("ready = %v, want false", body["ready"])
	}
	checks, ok := body["checks"].([]any)
	if !ok || len(checks) != 2 {
		t.Fatalf("checks = %v, want 2 entries", body["checks"])
	}
	var sawBroken bool
	for _, c := range checks {
		m := c.(map[string]any)
		if m["name"] == "broken" {
			sawBroken = true
			if m["ok"] != false {
				t.Fatalf("broken check reported ok = %v", m["ok"])
			}
			if m["error"] == nil || m["error"] == "" {
				t.Fatal("broken check carries no error text")
			}
		}
	}
	if !sawBroken {
		t.Fatalf("the failing check is not named in the body: %v", checks)
	}
}

// TestTCPCheckAgainstRealListener is what makes wiring a dependency one line:
// the same host:port the init containers probe, checked continuously instead of
// only at startup.
func TestTCPCheckAgainstRealListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()

	live := TCPCheck("dep", addr, nil)
	if err := live.Probe(context.Background()); err != nil {
		t.Fatalf("probe against a live listener: %v", err)
	}

	ln.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if live.Probe(context.Background()) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("probe still passes after the listener closed")
}

func TestTCPCheckEmptyAddrPasses(t *testing.T) {
	// An unconfigured dependency must not hold a pod not-ready: the caller wires
	// the check unconditionally and configuration decides whether it applies.
	c := TCPCheck("optional", "", nil)
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("empty addr should pass, got %v", err)
	}
}

func TestFuncCheck(t *testing.T) {
	tests := []struct {
		name    string
		ok      func() bool
		wantErr bool
	}{
		{"passing", func() bool { return true }, false},
		{"failing", func() bool { return false }, true},
		{"nil predicate passes", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := FuncCheck("dep", tc.ok).Probe(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestChecksRunConcurrently: readiness latency must be the slowest dependency,
// not their sum, or a component with several checks times out the kubelet probe.
func TestChecksRunConcurrently(t *testing.T) {
	const delay = 200 * time.Millisecond
	slow := func(context.Context) error {
		time.Sleep(delay)
		return nil
	}
	checks := []Check{
		{Name: "a", Probe: slow},
		{Name: "b", Probe: slow},
		{Name: "c", Probe: slow},
	}
	start := time.Now()
	results := evaluate(context.Background(), checks)
	elapsed := time.Since(start)

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if elapsed > 2*delay {
		t.Fatalf("evaluate took %v for 3x%v checks — they ran sequentially", elapsed, delay)
	}
}

// TestChecksHonourTheDeadline: a hung dependency must surface as not-ready, not
// as a probe that never returns.
func TestChecksHonourTheDeadline(t *testing.T) {
	hung := Check{Name: "hung", Probe: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	results := evaluate(context.Background(), []Check{hung})
	if len(results) != 1 || results[0].OK {
		t.Fatalf("hung check should fail, got %+v", results)
	}
	if elapsed := time.Since(start); elapsed > 2*readinessTimeout {
		t.Fatalf("evaluate took %v, want it bounded by %v", elapsed, readinessTimeout)
	}
}

func TestEvaluateNoChecks(t *testing.T) {
	if results := evaluate(context.Background(), nil); results != nil {
		t.Fatalf("no checks should yield nil, got %v", results)
	}
}
