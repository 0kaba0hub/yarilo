package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// pprofRoutes is every path the profilers can be reached at, split by which
// switch is supposed to open it.
var (
	executionRoutes = []string{
		"/debug/pprof/profile", "/debug/pprof/trace", "/debug/pprof/cmdline",
		"/debug/pprof/symbol", "/debug/pprof/allocs", "/debug/pprof/goroutine",
		"/debug/pprof/block", "/debug/pprof/mutex", "/debug/pprof/threadcreate",
	}
	heapRoute = "/debug/pprof/heap"
)

// registered reports whether the mux routes path to anything.
//
// Asked of the mux rather than by issuing a request: GET /debug/pprof/profile
// runs a CPU profile, and its default sample window is thirty seconds — a suite
// that tests these endpoints by calling them tests nothing for half a minute
// each. ServeMux.Handler returns the matched pattern, or "" when nothing
// matches, which is the question these tests are actually asking.
func registered(t *testing.T, h http.Handler, path string) bool {
	t.Helper()
	mux, ok := h.(*http.ServeMux)
	if !ok {
		t.Fatalf("telemetry handler is %T, not *http.ServeMux; this test can no longer see the routing table", h)
	}
	_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil))
	return pattern != ""
}

// status issues a real request, for the endpoints that answer immediately.
func status(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

// The one that matters. A diagnostic endpoint that is on when the config says
// off is not a missing feature, it is an open door nobody knows about — and it
// fails silently, which is why this is asserted rather than assumed.
func TestPprofIsAbsentUnlessEnabled(t *testing.T) {
	h := NewWithOptions(Options{Addr: ":0"}).Handler()

	for _, path := range append(executionRoutes, heapRoute, "/debug/pprof/") {
		if registered(t, h, path) {
			t.Errorf("%s is routed with pprof disabled", path)
		}
	}
}

// The switch has to actually do something, or the test above passes for the
// wrong reason.
func TestPprofIsPresentWhenEnabled(t *testing.T) {
	h := NewWithOptions(Options{Addr: ":0", Pprof: PprofOptions{Enabled: true}}).Handler()

	for _, path := range executionRoutes {
		if !registered(t, h, path) {
			t.Errorf("%s is not routed with pprof enabled", path)
		}
	}
}

// The heap dump is the one that can contain message bodies, so enabling the
// execution profilers must not bring it along. This is the assertion that
// keeps the split real: pprof.Index dispatches any unrouted suffix to the
// runtime profile of that name, so mounting the prefix instead of the
// individual routes would serve heap here and the split would exist only in
// the option.
func TestExecutionProfilesDoNotOpenTheHeapDump(t *testing.T) {
	h := NewWithOptions(Options{Addr: ":0", Pprof: PprofOptions{Enabled: true}}).Handler()

	if registered(t, h, heapRoute) {
		t.Errorf("%s is routed with only the execution profiles enabled", heapRoute)
	}
	// The index page would list and route to it, so it is not mounted at all.
	if registered(t, h, "/debug/pprof/") {
		t.Error("/debug/pprof/ is routed — the index dispatches to every runtime profile, heap included")
	}
}

// And the heap switch opens exactly that one.
func TestHeapSwitchOpensTheHeapDumpAlone(t *testing.T) {
	h := NewWithOptions(Options{Addr: ":0", Pprof: PprofOptions{Heap: true}}).Handler()

	if !registered(t, h, heapRoute) {
		t.Errorf("%s is not routed with the heap switch on", heapRoute)
	}
	for _, path := range executionRoutes {
		if registered(t, h, path) {
			t.Errorf("%s is routed with only the heap switch on", path)
		}
	}
}

// pprof shares the telemetry port with /metrics and the health endpoints, and
// #1008 was two containers assigned the same telemetry port — so "it is on the
// internal port" is an assumption worth asserting rather than trusting. The
// public listeners are separate servers; what this pins is that enabling the
// profilers does not disturb what the telemetry port already serves.
func TestPprofDoesNotDisturbTheExistingEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pprof PprofOptions
	}{
		{"disabled", PprofOptions{}},
		{"execution", PprofOptions{Enabled: true}},
		{"heap", PprofOptions{Enabled: true, Heap: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewWithOptions(Options{Addr: ":0", Pprof: tc.pprof}).Handler()
			for _, path := range []string{"/healthz", "/metrics"} {
				if got := status(t, h, path); got != http.StatusOK {
					t.Errorf("%s answers %d, want 200", path, got)
				}
			}
		})
	}
}
