// Package adminapi is the storage-plane admin HTTP API.
//
// It exposes operator endpoints (dict / acl / quota / folder / user)
// against the local backend's storage and pkg/dict instances. One
// instance runs per backend tag (or one per standalone deployment);
// in a multi-pod backend cluster the director's
// /api/admin/proxy/<user>/... transparently routes admin requests
// to the correct backend's adminapi via the same wire protocol.
//
// Wire protocol: JSON over HTTPS. Endpoints mirror the shape of
// internal/director's existing /api/director/... surface so the
// yarilo-admin CLI can speak both with identical machinery
// (bearer-token auth, IP allow-list, application/json bodies).
//
// Streaming endpoints (currently dict/iterate) use NDJSON — one
// JSON object per line — so the CLI can pipeline display without
// buffering the entire response in memory.
package adminapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/dict"
)

// Server is the admin-API HTTP server. Construct with New, then call
// Serve (mTLS-terminated TLS listener) or ListenAndServe (plain).
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// Options configures Server. Dicts is the live map opened by the
// host process (backend.New); the server hands out pointers to those
// objects via lookups keyed by name. Token is the shared admin
// secret; empty disables auth (test/dev only). AllowedNets, when
// non-empty, restricts which client IPs may reach the API.
type Options struct {
	Addr        string
	TLSConfig   *tls.Config
	Token       string
	AllowedNets []*net.IPNet
	Dicts       map[string]dict.Dict
}

// New constructs a Server and registers the admin endpoints onto an
// internal ServeMux. Subsequent Server.routes additions (for ACL,
// quota, etc.) happen via dedicated wire registration in their own
// files (acl.go, quota.go); dict.go owns the dict surface.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.registerHealth()
	s.registerDictRoutes()
	return s
}

// Serve starts the HTTP server. When opts.TLSConfig is non-nil the
// listener is TLS-terminated (use this in k8s deployments); plain
// TCP is for local dev. Blocks until ctx is cancelled or the
// underlying server returns.
func (s *Server) Serve(ctx context.Context) error {
	if s.opts.Addr == "" {
		return fmt.Errorf("adminapi: Addr is required")
	}
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         s.opts.TLSConfig,
	}
	errCh := make(chan error, 1)
	go func() {
		if s.opts.TLSConfig != nil {
			errCh <- srv.ListenAndServeTLS("", "")
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Handler returns the underlying mux — useful for tests that drive
// the server via httptest.Server without spinning up Serve.
func (s *Server) Handler() http.Handler { return s.mux }

// registerHealth wires GET /api/admin/health (200 if process is up).
// Readiness vs liveness is the operator's choice — both share this
// endpoint for now; per-subsystem readyz come later.
func (s *Server) registerHealth() {
	s.mux.Handle("GET /api/admin/health", s.middleware(func(w http.ResponseWriter, _ *http.Request) {
		apiJSON(w, map[string]string{"status": "ok"})
	}))
}

// middleware chains the IP allow-list and bearer-token checks. Use
// for every authenticated endpoint. Anonymous endpoints (none yet)
// should bypass and document why.
func (s *Server) middleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.opts.AllowedNets) > 0 {
			clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(clientIP)
			allowed := false
			for _, n := range s.opts.AllowedNets {
				if ip != nil && n.Contains(ip) {
					allowed = true
					break
				}
			}
			if !allowed {
				apiError(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		if s.opts.Token != "" {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != s.opts.Token {
				apiError(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	})
}

// apiJSON writes v as the response body with Content-Type
// application/json. Encoder failures are swallowed because the
// connection is already half-written; logging there would not help
// the caller. Use apiError instead for status-coded JSON errors.
func apiJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("adminapi: encode response", "err", err)
	}
}

// apiError writes a JSON {"error": msg} body with the supplied
// HTTP status. The status is set BEFORE the body so middleware
// (logger, metrics) can pick the right code.
func apiError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// decodeJSON reads r.Body into v with a 1 MiB cap; calls
// apiError on parse failure. Returns false to abort the handler.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		apiError(w, "read body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	if len(body) == 0 {
		apiError(w, "request body required", http.StatusBadRequest)
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		apiError(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
