package telemetry

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
)

// PprofOptions opts a component into the Go runtime profilers. Off unless a
// component enables it from config: this is a diagnostic switch an operator
// throws for the duration of an investigation, not a thing to leave on.
//
// The reason it is not merely "internal port, therefore harmless": a profile of
// a mail process is taken from a process that has just parsed other people's
// mail, and #1008 is recent evidence that which port a container serves on is
// an assumption worth asserting rather than trusting.
type PprofOptions struct {
	// Enabled registers the profilers that describe execution — CPU, execution
	// trace, and the allocation, goroutine, block and mutex profiles.
	Enabled bool
	// Heap additionally registers /debug/pprof/heap.
	//
	// Separate from Enabled because the two differ in kind, not degree. The
	// allocation profile is stacks and counts: where allocations were made, not
	// what was in them. The heap profile dumps the live objects of a process
	// whose live objects are messages — bodies, headers, and whatever
	// credentials a component holds. Splitting them means the common case,
	// finding where CPU and allocations go, never needs the dangerous one.
	Heap bool
}

// registerPprof installs the profiling endpoints on mux.
//
// The routes are registered one by one rather than by handing the whole
// /debug/pprof/ prefix to pprof.Index. Index dispatches any unrouted suffix to
// the runtime profile of that name, so mounting it would serve /debug/pprof/heap
// whatever Heap says — the split would exist in the option and not in the
// server. There is no index page for the same reason; the URLs are in the docs.
func registerPprof(mux *http.ServeMux, opts PprofOptions) {
	if !opts.Enabled && !opts.Heap {
		return
	}
	if opts.Enabled {
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		for _, name := range []string{"allocs", "goroutine", "block", "mutex", "threadcreate"} {
			mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
		}
	}
	if opts.Heap {
		mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	}

	// Warned at every start, not only in the config an operator read once. The
	// failure this guards against is not enabling it — it is enabling it for an
	// afternoon's investigation and leaving it on for a year.
	slog.Warn("telemetry: pprof endpoints are ENABLED — this is a diagnostic switch, turn it off when the investigation ends",
		"heap", opts.Heap,
		"exposure", pprofExposure(opts))
}

// pprofExposure states in the log what the operator has actually opened.
func pprofExposure(opts PprofOptions) string {
	if opts.Heap {
		return "execution profiles and /debug/pprof/heap, which dumps live objects and can contain message bodies"
	}
	return "execution profiles only (no heap dump)"
}
