package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

// profileRoutes is every path the profilers can be reached at. One switch
// opens all of them (#1488).
var profileRoutes = []string{
	"/debug/pprof/profile", "/debug/pprof/trace", "/debug/pprof/cmdline",
	"/debug/pprof/symbol", "/debug/pprof/allocs", "/debug/pprof/heap",
	"/debug/pprof/goroutine", "/debug/pprof/block", "/debug/pprof/mutex",
	"/debug/pprof/threadcreate",
}

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

	for _, path := range append(profileRoutes, "/debug/pprof/") {
		if registered(t, h, path) {
			t.Errorf("%s is routed with pprof disabled", path)
		}
	}
}

// The switch has to actually do something, or the test above passes for the
// wrong reason -- and it opens every profile, the heap route included.
//
// That route used to have a switch of its own, on the grounds that it dumps
// live objects while the allocation profile only records where allocations were
// made. Both halves were wrong: heap and allocs are the same runtime profile
// with different default sample types, so the allocation route already served
// inuse_space, and neither carries the contents of anything a pprof profile
// could hold (#1488).
func TestPprofIsPresentWhenEnabled(t *testing.T) {
	h := NewWithOptions(Options{Addr: ":0", Pprof: PprofOptions{Enabled: true}}).Handler()

	for _, path := range profileRoutes {
		if !registered(t, h, path) {
			t.Errorf("%s is not routed with the profilers enabled", path)
		}
	}
	// Still no index page: it dispatches any unrouted suffix to the runtime
	// profile of that name, so the served set would be decided by whatever any
	// package in the binary registered rather than by the list above.
	if registered(t, h, "/debug/pprof/") {
		t.Error("/debug/pprof/ is routed — the index dispatches to every registered runtime profile, not to this list")
	}
}

// An invented path answers 404, which is what separates "each route is
// registered by name" from "the prefix is mounted".
//
// The three assertions above cannot tell those apart: they check paths that a
// prefix mount would also serve, and the absence of the index page only says
// the index itself is unrouted. pprof.Index dispatches any unrouted suffix to
// the runtime profile of that name, so under a prefix mount this path would
// answer rather than 404 -- which is how the served set would come to be
// decided by whatever a package registered with runtime/pprof instead of by the
// list in registerPprof. QA added this control while verifying #1491; it is
// kept here so it outlives the report.
func TestAnInventedProfilePathIsNotRouted(t *testing.T) {
	h := NewWithOptions(Options{Addr: ":0", Pprof: PprofOptions{Enabled: true}}).Handler()

	for _, p := range []string{"/debug/pprof/zzz", "/debug/pprof/heap/extra", "/debug/pprof/allocsx"} {
		if registered(t, h, p) {
			t.Errorf("%s is routed — the profilers are mounted as a prefix, not as a list", p)
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

// The routing tests ask the mux, which is how they stay fast — but a handler
// wired correctly and a handler that produces anything are different claims. A
// CPU profile that returns an empty body, or a body that is not a profile,
// answers 200 either way.
func TestCPUProfileIsActuallyProduced(t *testing.T) {
	srv := NewWithOptions(Options{Addr: "127.0.0.1:0", Pprof: PprofOptions{Enabled: true}})
	ts := httptest.NewServer(srv.srv.Handler)
	defer ts.Close()

	// Burn a little CPU while the profile is being taken, so an empty result
	// means the profiler is not running rather than that nothing happened.
	stop := make(chan struct{})
	go func() {
		x := 0
		for {
			select {
			case <-stop:
				return
			default:
				x++
				_ = x
			}
		}
	}()
	defer close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/debug/pprof/profile?seconds=1", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET profile: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("the CPU profile is empty")
	}
	// A pprof profile is gzip-compressed protobuf; the magic is what tells a
	// real profile from an error page served with status 200.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("the response is not a pprof profile (first bytes %x)", body[:min(4, len(body))])
	}
	prof, err := profile.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if len(prof.Sample) == 0 {
		t.Error("the profile parsed but holds no samples: the profiler ran and recorded nothing")
	}
}

// restoreProfileRates puts both sampling rates back to off when the test ends.
// Both, not the one the caller happened to set: these are process-wide, so a
// row that leaves one on changes what the next row measures, and the next row
// is the one that would look wrong.
func restoreProfileRates(t *testing.T) {
	t.Cleanup(func() {
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	})
}

// A block profile route with no sampling rate answers 200 with a profile that
// has no samples — an answer-shaped silence. The rate is what makes it speak,
// so the two must be wired together.
func TestBlockProfileNeedsItsRate(t *testing.T) {
	restoreProfileRates(t)

	NewWithOptions(Options{Addr: "127.0.0.1:0", Pprof: PprofOptions{Enabled: true, BlockRate: 1}})

	// Block on a channel so there is something to sample.
	done := make(chan struct{})
	go func() {
		time.Sleep(5 * time.Millisecond)
		close(done)
	}()
	<-done

	var buf bytes.Buffer
	if err := pprof.Lookup("block").WriteTo(&buf, 0); err != nil {
		t.Fatalf("write block profile: %v", err)
	}
	prof, err := profile.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("parse block profile: %v", err)
	}
	if len(prof.Sample) == 0 {
		t.Error("block profiling was switched on and sampled nothing")
	}
}
