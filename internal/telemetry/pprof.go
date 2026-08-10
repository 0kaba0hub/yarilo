package telemetry

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime"
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
	// BlockRate sets runtime.SetBlockProfileRate: one blocking event in this
	// many nanoseconds of blocked time is sampled. 0 leaves the profile off,
	// which is the state it must be left in.
	//
	// It is separate from Enabled because registering the route and collecting
	// the samples are different costs. Without a rate, /debug/pprof/block
	// answers 200 with an empty profile — the worst shape a diagnostic can
	// take, since it looks like an answer. With one, every blocking operation
	// in the process pays for the sampling.
	BlockRate int
	// MutexFraction sets runtime.SetMutexProfileFraction: one in this many
	// mutex contention events is sampled. 0 leaves it off. Same reasoning as
	// BlockRate.
	MutexFraction int
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
	// The sampling rates are a runtime setting, not a route, so they are applied
	// even when nothing is registered: a component can be asked to collect
	// without also being asked to serve.
	applyProfileRates(opts)
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

// applyProfileRates turns on the sampling the block and mutex profiles need.
// Both are process-wide and both cost something on every blocking operation and
// every contended mutex, so they stay off unless an operator turns them on for
// a measurement and turns them off after.
func applyProfileRates(opts PprofOptions) {
	if opts.BlockRate > 0 {
		runtime.SetBlockProfileRate(opts.BlockRate)
		slog.Warn("telemetry: block profiling is ON — every blocking operation is now sampled; turn it off when the measurement ends",
			"rate_ns", opts.BlockRate)
	}
	if opts.MutexFraction > 0 {
		runtime.SetMutexProfileFraction(opts.MutexFraction)
		slog.Warn("telemetry: mutex profiling is ON — contention events are now sampled; turn it off when the measurement ends",
			"fraction", opts.MutexFraction)
	}
}

// pprofExposure states in the log what the operator has actually opened.
func pprofExposure(opts PprofOptions) string {
	if opts.Heap {
		return "execution profiles and /debug/pprof/heap, which dumps live objects and can contain message bodies"
	}
	return "execution profiles only (no heap dump)"
}
