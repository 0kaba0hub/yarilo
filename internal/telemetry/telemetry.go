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

	"github.com/0kaba0hub/yarilo/pkg/logging"
)

// logLevelDesc describes the active-log-level metric (#889), which an operator
// uses to confirm from the metrics they already scrape that a change took effect.
var logLevelDesc = prometheus.NewDesc(
	"yarilo_log_level",
	"Active log level (value = slog level number, label = name).",
	[]string{"level"}, nil,
)

// logLevelCollector reads the level at SCRAPE time rather than caching it in a
// gauge.
//
// This is deliberate: the level can change without any HTTP request touching
// this package — SetLevelFor's TTL reverts it from a timer inside pkg/logging,
// which cannot call back here without inverting the dependency. A cached gauge
// went stale exactly there, reporting the raised level after it had already
// reverted, which defeats the point of publishing it at all. Computing on
// collect makes staleness impossible instead of merely unlikely.
type logLevelCollector struct{}

func (logLevelCollector) Describe(ch chan<- *prometheus.Desc) { ch <- logLevelDesc }

func (logLevelCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		logLevelDesc, prometheus.GaugeValue, float64(logging.Level()), logging.String(),
	)
}

var logLevelOnce sync.Once

// publishLogLevel registers the collector once. Kept as a function so the call
// sites read the same as before.
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
}

// Addr resolves the telemetry listen address, letting the TELEMETRY_LISTEN env
// var override the config value. In the co-located backend pod (#788) every
// container shares the pod IP and reads the same yarilo.yaml, so each must be
// told a distinct telemetry port via env; non-co-located components leave it
// unset and fall back to the config value.
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
// endpoints from this one implementation; before unification each binary built
// its own mux, which is how /debug/loglevel ended up in two components out of
// fourteen and how /readyz ended up unconditional in eleven.
type Options struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Registry gathers the metrics to serve. Nil uses the default registry,
	// which is what promauto-based components register into.
	Registry prometheus.Gatherer
	// Checks are the component's readiness conditions. /readyz passes when every
	// one of them passes; an empty list means the process being up IS the
	// condition, which is a legitimate answer for a component with no external
	// dependency — but state it by leaving this empty on purpose, not by accident.
	//
	// Wiring a dependency is meant to be one line:
	//
	//	Checks: []telemetry.Check{
	//	    telemetry.TCPCheck("auth", authAddr, authTLS),
	//	    telemetry.TCPCheck("director", directorAddr, directorTLS),
	//	}
	Checks []Check
	// Lifecycle makes /readyz additionally require SetReady(true). Components
	// that go through a startup phase, or that mark themselves unready while
	// draining, set this; without it the flag is ignored so a component that
	// never calls SetReady is not stuck at not-ready forever.
	Lifecycle bool
}

// New creates a telemetry server listening on addr, serving the default registry
// and gating /readyz on the SetReady flag.
func New(addr string) *Server {
	return NewWithOptions(Options{Addr: addr, Lifecycle: true})
}

// NewWithOptions creates a telemetry server from explicit options.
func NewWithOptions(opts Options) *Server {
	s := &Server{checks: opts.Checks, lifecycle: opts.Lifecycle}

	metrics := promhttp.Handler()
	if opts.Registry != nil {
		metrics = promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/debug/loglevel", s.logLevel)
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

// IsReady reports the current readiness — the /readyz condition. The
// backend registration client (#776) gates its heartbeat on this so a
// not-ready backend stops heartbeating and is expired ring-wide rather
// than being kept as a live-but-wedged routing target.
func (s *Server) IsReady() bool { return s.isReady() }

// ListenAndServe starts the HTTP server. Blocks until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
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
	// The body names the failing dependency: "not ready" alone sends an operator
	// reading kubectl describe on a hunt that the pod could have answered.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":  ready,
		"checks": results,
	})
}

// logLevel reads or changes the active log level (#889).
//
//	GET  /debug/loglevel                       → {"level":"info"}
//	POST /debug/loglevel {"level":"debug"}     → change until further notice
//	POST /debug/loglevel {"level":"debug","ttl":"30s"} → revert automatically
//
// This listener is the telemetry port, which is not exposed to mail clients; it
// must never be published on a client-facing service. The TTL form is the one to
// prefer: a bounded raise cannot be forgotten in the on position, which is how a
// debug switch usually ends up rotating away the log it was meant to capture.
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
