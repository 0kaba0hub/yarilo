// Package jmaplogin is the JMAP login proxy: client TLS, auth, warden
// accounting, director lookup, then a proxied request. The hop it fronts is
// per-request HTTP rather than a byte pipe, so identity travels in headers and
// nothing is handed over as a file descriptor.
package jmaplogin

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/yarilomail/yarilo/internal/auth/oauth2"
)

// service is the name this proxy reports to yarilo-auth and yarilo-warden.
const service = "jmap"

// Options wires the proxy. A nil Auth or TokenValidator disables that
// credential type rather than failing startup, so a deployment can offer one.
type Options struct {
	Addr string
	// ExtTLS is the client-facing certificate. Nil serves plain HTTP, which is
	// only sensible behind a TLS-terminating ingress.
	ExtTLS *tls.Config

	Auth           PasswordAuthenticator
	TokenValidator oauth2.Validator
	// DisablePlainAuth refuses Basic over a connection that is not TLS-protected.
	DisablePlainAuth bool

	// Router resolves a username to its backend address.
	Router Router
	// BackendTLS is the internal mTLS config for the hop to yarilo-jmap. Nil
	// means plain HTTP, which the backend only accepts inside a trusted network.
	BackendTLS *tls.Config

	// Warden accounts connections per user@IP. Nil disables accounting.
	Warden ConnAccountant
	// WardenFailOpen allows the connection when warden is unreachable.
	WardenFailOpen bool

	ProxyProtocol      bool
	HAProxyTrustedNets []*net.IPNet
	HAProxyTimeout     time.Duration

	// LocalIP is this pod's address, reported to the backend in Forwarded.
	LocalIP string
}

// ConnAccountant is warden's per-user@IP connection accounting, narrowed to
// what this proxy calls. Satisfied by *warden.Pool.
type ConnAccountant interface {
	Connect(id, user, ip, service string) error
	Disconnect(id, user, ip, service string) error
}

// Router resolves a user to the backend that owns their state.
type Router interface {
	// Backend returns the "host:port" serving username. sessionID is passed
	// through to the director so a held LOOKUP can be correlated.
	Backend(username, sessionID string) (string, error)
}

// PasswordAuthenticator verifies a username and password, returning the
// resolved username. Satisfied by the yarilo-auth client.
type PasswordAuthenticator interface {
	Authenticate(username, password, service, remoteIP, sessionID string) (string, error)
}

// connState is the per-TCP-connection warden accounting. HTTP has no session
// bound to a connection, so the connection is the unit that can be counted and,
// later, kicked; a request is not.
type connState struct {
	mu sync.Mutex
	// id is this connection's warden session id, allocated once.
	id string
	ip string
	// user is who the connection is currently accounted as. Empty means it has
	// never authenticated, and such a connection is never accounted: brute
	// force on it is yarilo-auth's penalty to handle, not a connection count.
	user string
}

type ctxKey struct{}

// Server is the JMAP login proxy.
type Server struct {
	opts  Options
	mux   *http.ServeMux
	proxy *backendProxy

	// conns maps a live connection to its accounting state so the ConnState
	// hook can release it; ConnContext cannot see the close.
	conns sync.Map // net.Conn -> *connState
}

// New builds the proxy.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.proxy = newBackendProxy(opts)
	s.mux.HandleFunc("/", s.handle)
	return s
}

// Handler exposes the routes for tests without binding a port.
func (s *Server) Handler() http.Handler { return s.mux }

// Serve runs until ctx is cancelled, then drains in-flight requests.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux,
		TLSConfig:         s.opts.ExtTLS,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			st := &connState{ip: hostOnly(c.RemoteAddr().String())}
			s.conns.Store(c, st)
			return context.WithValue(ctx, ctxKey{}, st)
		},
		ConnState: func(c net.Conn, state http.ConnState) {
			if state != http.StateClosed && state != http.StateHijacked {
				return
			}
			if v, ok := s.conns.LoadAndDelete(c); ok {
				s.releaseConn(v.(*connState))
			}
		},
	}

	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("jmap-login: listen %s: %w", s.opts.Addr, err)
	}
	// The PROXY header precedes TLS, so this wrapper is innermost.
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

	errCh := make(chan error, 1)
	go func() {
		if s.opts.ExtTLS != nil {
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

// handle authenticates, accounts and proxies one request.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	st, _ := r.Context().Value(ctxKey{}).(*connState)
	clientIP := clientAddr(r, st)

	username, err := s.authenticate(r, clientIP, sessionOf(st))
	if err != nil {
		// The reason is logged, never sent: telling a caller whether the user
		// exists turns this endpoint into an account oracle.
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap", Bearer`)
		writeProblem(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if err := s.account(st, username, clientIP); err != nil {
		writeProblem(w, http.StatusServiceUnavailable, "Connection limit reached")
		return
	}

	backend, err := s.opts.Router.Backend(username, sessionOf(st))
	if err != nil {
		slog.Warn("jmap-login: backend lookup failed", "user", username, "err", err)
		writeProblem(w, http.StatusBadGateway, "No backend for this user")
		return
	}
	s.proxy.serve(w, r, backend, username, sessionOf(st), clientIP)
}

// account keeps warden's per-user@IP view in step with this connection. A
// connection that authenticates as a different user, which HTTP permits on a
// keep-alive, is re-accounted rather than refused or counted twice.
func (s *Server) account(st *connState, username, ip string) error {
	if s.opts.Warden == nil || st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.user == username {
		return nil
	}
	if st.id == "" {
		st.id = newSessionID()
	}
	if st.user != "" {
		if err := s.opts.Warden.Disconnect(st.id, st.user, st.ip, service); err != nil {
			slog.Warn("jmap-login: warden disconnect failed", "user", st.user, "err", err)
		}
		st.user = ""
	}
	if err := s.opts.Warden.Connect(st.id, username, ip, service); err != nil {
		if !s.opts.WardenFailOpen {
			slog.Warn("jmap-login: warden connect refused", "user", username, "ip", ip, "err", err)
			return err
		}
		// A warden blip must not lock everyone out; the count is approximate
		// until it recovers.
		slog.Warn("jmap-login: warden unreachable, allowing", "user", username, "err", err)
	}
	st.user = username
	st.ip = ip
	return nil
}

// releaseConn drops the connection's warden accounting. A connection that never
// authenticated holds none.
func (s *Server) releaseConn(st *connState) {
	if s.opts.Warden == nil {
		return
	}
	st.mu.Lock()
	user, id, ip := st.user, st.id, st.ip
	st.user = ""
	st.mu.Unlock()
	if user == "" {
		return
	}
	if err := s.opts.Warden.Disconnect(id, user, ip, service); err != nil {
		slog.Warn("jmap-login: warden disconnect failed", "user", user, "err", err)
	}
}

func sessionOf(st *connState) string {
	if st == nil {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.id == "" {
		st.id = newSessionID()
	}
	return st.id
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A duplicate id only blurs correlation in logs; refusing the
		// connection over it would be worse.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// clientAddr is the peer address the passdb and warden see. The PROXY header
// has already rewritten it at the listener; request headers are never consulted,
// since a client choosing its own source address defeats allow_nets.
func clientAddr(r *http.Request, st *connState) string {
	if st != nil && st.ip != "" {
		return st.ip
	}
	return hostOnly(r.RemoteAddr)
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// proxyPolicy trusts a PROXY header only from a configured upstream. An empty
// list trusts nobody rather than everybody.
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
