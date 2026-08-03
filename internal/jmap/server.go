package jmap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	"golang.org/x/net/netutil"

	"github.com/yarilomail/yarilo/pkg/config"
)

// Well-known endpoint of RFC 8620 §2.2. A client is given only this URL and
// discovers everything else from the session resource it returns.
const sessionPath = "/.well-known/jmap"

// Options wires the server. A nil Auth or TokenValidator disables that
// credential type rather than failing at startup, so a deployment can offer
// only one.
type Options struct {
	Addr             string
	Protocol         config.JMAPProtocolConfig
	TLSConfig        *tls.Config
	Auth             PasswordAuthenticator
	TokenValidator   oauth2Validator
	DisablePlainAuth bool
	// ConnectionLimit caps concurrent connections. 0 is unlimited.
	ConnectionLimit int
	// ProxyProtocol reads a HAProxy PROXY header before TLS, so the passdb's
	// allow_nets check sees the real client rather than the proxy.
	ProxyProtocol      bool
	HAProxyTrustedNets []*net.IPNet
	HAProxyTimeout     time.Duration
}

// Server serves the JMAP endpoint.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New builds the server and registers its routes.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET "+sessionPath, s.handleSession)
	return s
}

// Handler exposes the routes for httptest without binding a port.
func (s *Server) Handler() http.Handler { return s.mux }

// Serve runs until ctx is cancelled, then drains in-flight requests.
func (s *Server) Serve(ctx context.Context) error {
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
	// PROXY header is pre-TLS, so this wrapper stays innermost; the limiter
	// sits outside it so a refused connection is never parsed.
	if s.opts.ProxyProtocol {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			ReadHeaderTimeout: timeout,
			Policy:            proxyPolicy(s.opts.HAProxyTrustedNets),
		}
	}
	if s.opts.ConnectionLimit > 0 {
		ln = netutil.LimitListener(ln, s.opts.ConnectionLimit)
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

// handleSession answers the session resource. Authentication happens here
// rather than in a wrapper because this is currently the only route; the
// request-level plumbing arrives with the API endpoint.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	username, err := s.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap", Bearer`)
		writeProblem(w, http.StatusUnauthorized, "about:blank", "Unauthorized")
		return
	}
	sess := buildSession(s.opts.Protocol, username)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	writeJSON(w, http.StatusOK, sess)
}

// writeJSON emits a response body. A failure mid-write cannot be signalled to
// the client, the status line is already gone, so it is only logged.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("jmap: response write failed", "err", err)
	}
}

// writeProblem emits the RFC 7807 problem details JMAP uses for request-level
// errors (RFC 8620 §3.6.1).
func writeProblem(w http.ResponseWriter, status int, problemType, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := map[string]any{"type": problemType, "status": status, "detail": detail}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("jmap: problem write failed", "err", err)
	}
}

// logAuthFailure records why a credential was rejected. The response says only
// "unauthorized", so this is the sole place the reason survives.
func (s *Server) logAuthFailure(method, username string, err error) {
	slog.Info("jmap: auth failed", "method", method, "user", username, "err", err)
}

// proxyPolicy trusts a PROXY header only from a configured upstream. An empty
// list ignores headers entirely rather than trusting everyone, since a client
// that can set its own source address defeats allow_nets.
func proxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcp, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcp.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}
