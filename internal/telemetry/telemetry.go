// Package telemetry exposes /healthz, /readyz, and /metrics on a dedicated port.
package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/0kaba0hub/yarilo/pkg/logging"
)

// logLevelGauge publishes the active log level so an operator can confirm from
// the same place they read every other metric that a change took effect (#889).
// The value is slog's numeric level; the label carries the name.
var logLevelGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "yarilo",
	Name:      "log_level",
	Help:      "Active log level (value = slog level number, label = name).",
}, []string{"level"})

func publishLogLevel() {
	logLevelGauge.Reset()
	logLevelGauge.WithLabelValues(logging.String()).Set(float64(logging.Level()))
}

// Server is the telemetry HTTP server.
type Server struct {
	srv   *http.Server
	ready atomic.Bool
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

// New creates a telemetry server listening on addr (e.g. ":8080").
func New(addr string) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/debug/loglevel", s.logLevel)
	mux.Handle("/metrics", promhttp.Handler())
	publishLogLevel()
	s.srv = &http.Server{Addr: addr, Handler: mux}
	return s
}

// SetReady marks the server as ready to serve traffic.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// IsReady reports the current readiness — the /readyz condition. The
// backend registration client (#776) gates its heartbeat on this so a
// not-ready backend stops heartbeating and is expired ring-wide rather
// than being kept as a live-but-wedged routing target.
func (s *Server) IsReady() bool { return s.ready.Load() }

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

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if s.ready.Load() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("not ready")) //nolint:errcheck
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
