// Package telemetry exposes /healthz, /readyz, and /metrics on a dedicated port.
package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yarilomail/yarilo/pkg/logging"
)

// logLevelDesc describes the active-log-level metric, letting an operator
// confirm from scraped metrics that a level change took effect.
var logLevelDesc = prometheus.NewDesc(
	"yarilo_log_level",
	"Active log level (value = slog level number, label = name).",
	[]string{"level"}, nil,
)

// logLevelCollector reads the level at scrape time rather than caching a gauge:
// SetLevelFor's TTL can revert the level from a timer inside pkg/logging without
// any HTTP request here, so a cached gauge would go stale. Computing on collect
// makes staleness impossible.
type logLevelCollector struct{}

func (logLevelCollector) Describe(ch chan<- *prometheus.Desc) { ch <- logLevelDesc }

func (logLevelCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		logLevelDesc, prometheus.GaugeValue, float64(logging.Level()), logging.String(),
	)
}

var logLevelOnce sync.Once

// publishLogLevel registers the collector once.
func publishLogLevel() {
	logLevelOnce.Do(func() {
		prometheus.DefaultRegisterer.MustRegister(logLevelCollector{})
	})
}

// Server is the telemetry HTTP server.
type Server struct {
	srv       *http.Server
	ready     atomic.Bool
	checks    []Check
	lifecycle bool
	wd        *watchdog
	fault     *Gate
}

// Addr resolves the telemetry listen address; TELEMETRY_LISTEN overrides the
// config value. In the co-located backend pod every container shares the pod IP
// and reads the same yarilo.yaml, so each is told a distinct telemetry port via
// env; other components leave it unset and fall back to the config value.
func Addr(cfgListen string) string {
	if v := os.Getenv("TELEMETRY_LISTEN"); v != "" {
		return v
	}
	if cfgListen == "" {
		return ":8080"
	}
	return cfgListen
}

// Options configures a telemetry server. Every component serves the same four
// endpoints from this one implementation.
type Options struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Registry gathers the metrics to serve. Nil uses the default registry,
	// which is what promauto-based components register into.
	Registry prometheus.Gatherer
	// Checks are the component's readiness conditions: /readyz passes when every
	// one passes. An empty list means the process being up IS the condition — a
	// legitimate answer for a component with no external dependency, but leave it
	// empty on purpose, not by accident.
	//
	//	Checks: []telemetry.Check{
	//	    telemetry.TCPCheck("auth", authAddr, authTLS),
	//	    telemetry.TCPCheck("director", directorAddr, directorTLS),
	//	}
	Checks []Check
	// Lifecycle makes /readyz additionally require SetReady(true). Set it on
	// components with a startup phase or that mark themselves unready while
	// draining; without it the flag is ignored so a component that never calls
	// SetReady is not stuck not-ready forever.
	Lifecycle bool
	// Watchdog opts into timer-driven liveness: when the Check fails
	// FailureThreshold times in a row, /healthz starts failing so the kubelet
	// restarts a wedged-but-alive process. Left zero, /healthz stays
	// unconditional — this covers only deadlock / hung-storage states.
	Watchdog WatchdogOptions
	// Fault, when non-nil, registers POST /debug/fault/deadlock to wedge this
	// gate so a live pod can be driven into the tripped state to confirm the
	// watchdog end to end. Off unless the component opts in via config; the same
	// gate is what the watchdog Check enters.
	Fault *Gate
}

// New creates a telemetry server listening on addr, serving the default registry
// and gating /readyz on the SetReady flag.
func New(addr string) *Server {
	return NewWithOptions(Options{Addr: addr, Lifecycle: true})
}

// NewWithOptions creates a telemetry server from explicit options.
func NewWithOptions(opts Options) *Server {
	s := &Server{checks: opts.Checks, lifecycle: opts.Lifecycle, wd: newWatchdog(opts.Watchdog), fault: opts.Fault}

	metrics := promhttp.Handler()
	if opts.Registry != nil {
		metrics = promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/debug/loglevel", s.logLevel)
	if s.fault != nil {
		mux.HandleFunc("/debug/fault/deadlock", s.faultHandler)
	}
	mux.Handle("/metrics", metrics)
	publishLogLevel()
	s.srv = &http.Server{Addr: opts.Addr, Handler: mux}
	return s
}

// SetReady marks the server as ready to serve traffic.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// isReady resolves the lifecycle gate only. Dependency checks are evaluated per
// request in readyz, since they involve I/O.
func (s *Server) isReady() bool {
	if s.lifecycle {
		return s.ready.Load()
	}
	return true
}

// IsReady reports the current /readyz condition. The backend registration
// client gates its heartbeat on this so a not-ready backend stops heartbeating
// and is expired ring-wide rather than kept as a live-but-wedged target.
func (s *Server) IsReady() bool { return s.isReady() }

// ListenAndServe starts the HTTP server. Blocks until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.wd != nil {
		go s.wd.run(ctx)
	}
	go func() {
		<-ctx.Done()
		s.srv.Shutdown(context.Background()) //nolint:errcheck
	}()
	if err := s.srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Handler returns the HTTP handler used by the server (for testing).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	// A tripped watchdog is the liveness signal that restarts the container: the
	// process is up and this handler runs, but its request path is wedged.
	if s.wd.unhealthy() {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("watchdog: liveness self-check failing\n")) //nolint:errcheck
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok")) //nolint:errcheck
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	results := evaluate(r.Context(), s.checks)
	failed := make([]string, 0, len(results))
	for _, res := range results {
		if !res.OK {
			failed = append(failed, res.Name)
		}
	}
	ready := s.isReady() && len(failed) == 0

	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	// The body names the failing dependency so an operator reading kubectl
	// describe sees which check failed.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":  ready,
		"checks": results,
	})
}

// logLevel reads or changes the active log level.
//
//	GET  /debug/loglevel                       → {"level":"info"}
//	POST /debug/loglevel {"level":"debug"}     → change until further notice
//	POST /debug/loglevel {"level":"debug","ttl":"30s"} → revert automatically
//
// Served on the telemetry port only; must never be published on a client-facing
// service. Prefer the TTL form so a bounded raise cannot be left on forever.
func (s *Server) logLevel(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeLogLevel(w)
	case http.MethodPost, http.MethodPut:
		var req struct {
			Level string `json:"level"`
			TTL   string `json:"ttl"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.Level == "" {
			http.Error(w, `"level" is required`, http.StatusBadRequest)
			return
		}
		lvl := logging.ParseLevel(req.Level)
		if logging.LevelName(lvl) != normaliseLevelName(req.Level) {
			http.Error(w, "unknown level (want debug|info|warn|error)", http.StatusBadRequest)
			return
		}
		var ttl time.Duration
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil || d < 0 {
				http.Error(w, `"ttl" must be a duration such as "30s"`, http.StatusBadRequest)
				return
			}
			ttl = d
		}
		if ttl > 0 {
			logging.SetLevelFor(lvl, ttl)
			slog.Info("logging: level raised temporarily", "level", logging.String(), "ttl", ttl.String())
		} else {
			logging.SetLevel(lvl)
			slog.Info("logging: level changed", "level", logging.String())
		}
		publishLogLevel()
		writeLogLevel(w)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// normaliseLevelName lets the handler reject an unknown name instead of silently
// accepting it as info, which ParseLevel does by design for LOG_LEVEL.
func normaliseLevelName(s string) string {
	switch lower := strings.ToLower(strings.TrimSpace(s)); lower {
	case "warning":
		return "warn"
	default:
		return lower
	}
}

func writeLogLevel(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"level": logging.String()})
}
