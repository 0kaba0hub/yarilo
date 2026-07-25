// Package telemetry exposes /healthz, /readyz, and /metrics on a dedicated port.
package telemetry

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is the telemetry HTTP server.
type Server struct {
	srv   *http.Server
	ready atomic.Bool
}

// New creates a telemetry server listening on addr (e.g. ":8080").
func New(addr string) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.Handle("/metrics", promhttp.Handler())
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
