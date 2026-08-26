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
	// HeapDeprecated is the former separate switch for /debug/pprof/heap. It
	// changes nothing and is read only to warn: the route is now served by
	// Enabled with the rest.
	//
	// It was justified by two claims, both wrong (#1488). Go's heap and allocs
	// profiles are the same profile written twice, differing only in which
	// sample type is the default -- allocs already carries inuse_objects and
	// inuse_space, so the switch withheld nothing that Enabled did not already
	// serve. And neither profile carries the contents of anything: a pprof heap
	// profile is sampled stack traces with object and byte counts, and the
	// format has no field a message body could appear in.
	//
	// A boundary that is documented but not real is worse than none, because it
	// is trusted.
	HeapDeprecated bool
}

// registerPprof installs the profiling endpoints on mux.
//
// The routes are registered one by one rather than by handing the whole
// /debug/pprof/ prefix to pprof.Index. Index dispatches any unrouted suffix to
// the runtime profile of that name, so the set of routes would be whatever any
// package in the binary happened to register with runtime/pprof — decided by
// an import, not by this list. There is no index page for the same reason; the
// URLs are in the docs.
func registerPprof(mux *http.ServeMux, opts PprofOptions) {
	// The sampling rates are a runtime setting, not a route, so they are applied
	// even when nothing is registered: a component can be asked to collect
	// without also being asked to serve.
	applyProfileRates(opts)
	if opts.HeapDeprecated {
		slog.Warn("telemetry: telemetry_pprof_heap_enabled is deprecated and does nothing — /debug/pprof/heap is served by telemetry_pprof_enabled, which always could serve the same data through /debug/pprof/allocs (#1488); remove the key")
	}
	if !opts.Enabled {
		return
	}
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	// heap and allocs are the same profile with different default sample
	// types. Both are routed because the tools default to different ones.
	for _, name := range []string{"allocs", "heap", "goroutine", "block", "mutex", "threadcreate"} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}

	// Warned at every start, not only in the config an operator read once. The
	// failure this guards against is not enabling it — it is enabling it for an
	// afternoon's investigation and leaving it on for a year.
	slog.Warn("telemetry: pprof endpoints are ENABLED — this is a diagnostic switch, turn it off when the investigation ends",
		"exposure", pprofExposure())
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
//
// Not message contents: a profile is stack traces with counts. What it does
// give away is the shape of the process -- which code paths run, how often,
// and how much they allocate -- and the symbol names of the binary.
func pprofExposure() string {
	return "stacks, counts and symbol names — the code paths this process runs and how much they cost, not the contents of anything"
}
