// Package jmap is the JMAP backend: the pod that owns one user's state and
// answers the JMAP methods for it. It performs no authentication of its own —
// yarilo-jmap-login has already run the passdb chain and asserts the user in a
// request header — so everything here rests on the trust gate in trust.go.
//
// The protocol layer itself lives in pkg/jmapcore, which knows nothing about
// yarilo.
package jmap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// Options wires the backend.
type Options struct {
	Addr string
	// TLSConfig is the internal mTLS server config. Nil serves plain HTTP,
	// which only the trusted-nets mode can make safe.
	TLSConfig *tls.Config
	// Trust decides which peers may speak for a user.
	Trust Trust
	// Limits are the bounds advertised in the session resource.
	Limits jmapcore.Limits
	// OnListen fires once the port is bound. The co-located pod publishes its
	// readiness from it, so it must not run before the listener exists.
	OnListen func()
}

// Server serves the JMAP endpoint behind the login proxy.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New builds the server and registers its routes.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET "+jmapcore.SessionPath, s.guard(s.handleSession))
	return s
}

// Handler exposes the routes for tests without binding a port.
func (s *Server) Handler() http.Handler { return s.mux }

// Serve runs until ctx is cancelled, then drains in-flight requests.
func (s *Server) Serve(ctx context.Context) error {
	// An unanchored backend keeps its port bound and answers 403 with a named
	// cause. A dead port reads as a network fault and misdirects the operator.
	if s.opts.Trust.Mode == TrustNone {
		slog.Error("jmap: no trust anchor for the login hop — set internal_tls or xclient.trusted_nets; every request will be refused")
	}
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux,
		TLSConfig:         s.opts.TLSConfig,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("jmap: listen %s: %w", s.opts.Addr, err)
	}
	if s.opts.OnListen != nil {
		s.opts.OnListen()
	}
	errCh := make(chan error, 1)
	go func() {
		if s.opts.TLSConfig != nil {
			errCh <- srv.ServeTLS(ln, "", "")
			return
		}
		errCh <- srv.Serve(ln)
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

// handlerFunc is a route that has already been given a resolved identity.
type handlerFunc func(http.ResponseWriter, *http.Request, identity)

// guard runs the trust gate and the contract headers before any route. Every
// identity-bearing route goes through it; there is no path that reads
// X-Yarilo-User without it.
func (s *Server) guard(next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.Trust.allows(r) {
			slog.Warn("jmap: identity refused from untrusted peer",
				"peer", hostOnly(r.RemoteAddr), "mode", s.opts.Trust.Mode.String())
			jmapcore.WriteProblem(w, http.StatusForbidden, "Untrusted peer")
			return
		}
		id, err := readIdentity(r)
		if err != nil {
			slog.Warn("jmap: bad forwarded identity", "peer", hostOnly(r.RemoteAddr), "err", err)
			jmapcore.WriteProblem(w, http.StatusForbidden, "Invalid forwarded identity")
			return
		}
		// A budget already at zero means the request has been round-tripping
		// between proxies; answering it would extend the loop.
		if id.ttl == 0 {
			slog.Warn("jmap: proxy ttl exhausted", "user", id.user, "session", id.sessionID)
			jmapcore.WriteProblem(w, http.StatusLoopDetected, "Proxy TTL exhausted")
			return
		}
		next(w, r, id)
	}
}

// handleSession answers the session resource (RFC 8620 §2.2).
func (s *Server) handleSession(w http.ResponseWriter, _ *http.Request, id identity) {
	slog.Debug("jmap: session", "user", id.user, "session", id.sessionID, "client_ip", id.clientIP)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	jmapcore.WriteJSON(w, http.StatusOK, jmapcore.BuildSession(s.opts.Limits, id.user))
}
