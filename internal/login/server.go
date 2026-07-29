// Package login implements the yarilo mail-protocol login proxy.
// Each login pod accepts mail-client connections (IMAP, POP3, or SMTP Submission),
// extracts the protocol preamble to learn the authenticated username, queries the
// yarilo-director LOOKUP to find the correct backend pod, and proxies the session.
// TLS is terminated here; backends receive plain TCP (or mTLS for internal links).
package login

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/anvil"
	authclient "github.com/0kaba0hub/yarilo/internal/auth/client"
	"github.com/0kaba0hub/yarilo/internal/cluster/proto"
	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/pkg/retry"
)

// Protocol identifies the mail protocol handled by the login pod.
type Protocol string

const (
	ProtocolIMAP        Protocol = "imap"
	ProtocolIMAPS       Protocol = "imaps"
	ProtocolPOP3        Protocol = "pop3"
	ProtocolPOP3S       Protocol = "pop3s"
	ProtocolSubmission  Protocol = "submission"
	ProtocolSubmissions Protocol = "submissions"
	ProtocolManageSieve Protocol = "managesieve"
)

// Base collapses a listener protocol to the co-located backend container it maps
// to (imaps→imap, pop3s→pop3, submissions→submission) — the granularity the
// director counts sessions at for least_sessions placement (#797). Sent as the
// trailing proto field on LOOKUP / SESSION-OPEN.
func (p Protocol) Base() string {
	switch p {
	case ProtocolIMAPS:
		return "imap"
	case ProtocolPOP3S:
		return "pop3"
	case ProtocolSubmissions:
		return "submission"
	default:
		return string(p)
	}
}

// Options configures the login proxy Server.
type Options struct {
	// Protocol is one of the Protocol constants above.
	Protocol Protocol
	// Tag restricts director LOOKUP to backends with this tag (#737).
	// "" = the untagged pool, not "any tag" — there is no full-ring mode.
	Tag string
	// DirectorAddr is the host:port of yarilo-director (e.g. "yarilo-director:9102").
	// Ignored when BackendAddr is set.
	DirectorAddr string
	// DirectorTLS is the mTLS config for connecting to yarilo-director.
	// Nil means plain TCP.
	DirectorTLS *tls.Config
	// BackendAddr bypasses director LOOKUP entirely and routes every session to
	// this fixed address (e.g. "yarilo-imap:143" in standalone deployments).
	// When set, DirectorAddr and Tag are not used.
	BackendAddr string
	// LocalIP is the pod IP used in the ME handshake with the director.
	LocalIP string
	// BackendPort is the containerPort on backend pods.
	BackendPort int
	// BackendTLS is the mTLS config for connecting to backend pods.
	// Nil means plain TCP.
	BackendTLS *tls.Config
	// ExtTLS is the client-facing TLS config for implicit-TLS listeners
	// (IMAPS :993, POP3S :995, Submissions :465).
	// Nil means the listener is plain-text (no implicit TLS on accept).
	ExtTLS *tls.Config
	// StarttlsTLS is the TLS config offered via STARTTLS / STLS during the
	// preamble phase (IMAP :143, POP3 :110, Submission :587).
	// Nil means STARTTLS is not advertised or available on this listener.
	StarttlsTLS *tls.Config
	// AnvilAddr is the host:port of yarilo-anvil for per-user@IP connection
	// limiting (mail_max_userip_connections). Empty = no limit enforcement.
	AnvilAddr string
	// AnvilTLS is the mTLS config for connecting to yarilo-anvil.
	// Nil means plain TCP.
	AnvilTLS *tls.Config
	// AnvilFailOpen controls what happens when yarilo-anvil is unreachable.
	// true = allow the session (fail open); false = reject the session (fail closed).
	AnvilFailOpen bool
	// TransientRetries is how many extra attempts a transient failure gets before
	// the client is told the service is unavailable (#896). 0 selects the default
	// (3). Applies to failures that are temporary by definition — yarilo-auth
	// reporting temp-fail, the first dial to auth, and bringing up the backend
	// session — where answering on the first error turns a blip into a visible
	// login failure the client can only recover from by reconnecting.
	TransientRetries int
	// AnvilConns is how many long-lived connections the shared anvil pool keeps
	// (#878). 0 selects anvil.DefaultPoolSize. The anvil protocol carries no
	// request id, so a connection serves one command at a time; each command is a
	// sub-millisecond round trip, so a handful covers any realistic login rate.
	AnvilConns int
	// DialRetries is the number of attempts (with exponential backoff) when
	// dialling external dependencies at startup. 0 or 1 means a single attempt.
	DialRetries int

	// LookupHoldMax / LookupHoldBackoff bound the confirmed-kick LOOKUP retry
	// (#847/#858). Their product is the hold budget, which must exceed the
	// director's worst-case confirm time. 0 uses the package defaults (20 / 150ms
	// → 3s budget). From login.lookup_hold_max / lookup_hold_backoff_ms.
	LookupHoldMax     int
	LookupHoldBackoff time.Duration

	// AuthAddr is the host:port of yarilo-auth (e.g. "yarilo-auth:9100").
	// Required: if empty every login attempt is rejected with a temporary error.
	AuthAddr string
	// AuthTLS is the mTLS config for connecting to yarilo-auth.
	// Nil means plain TCP.
	AuthTLS *tls.Config

	// AuthMaxAttempts is the maximum number of failed authentication
	// attempts allowed on a single connection before the server sends
	// BYE and closes. 0 means use the default (3). Mirrors cfg.Auth.MaxAttempts.
	AuthMaxAttempts int

	// OAuth2Enabled advertises and accepts OAUTHBEARER and XOAUTH2 mechanisms.
	// Mirrors cfg.Auth.OAuth2 being non-empty.
	OAuth2Enabled bool
	// DisablePlainAuth suppresses PLAIN and LOGIN from pre-TLS capability
	// advertisements. After STARTTLS/implicit-TLS they are always offered.
	DisablePlainAuth bool
	// SieveExtensions is the space-joined list of supported Sieve extensions
	// advertised in the ManageSieve SIEVE capability line.
	SieveExtensions string
	// SieveMaxInvalidCmds is the number of unrecognised pre-auth commands
	// after which the server disconnects with BYE.
	SieveMaxInvalidCmds int

	// HAProxy enables PROXY protocol v1/v2 header reading from trusted upstreams.
	HAProxy        bool
	HAProxyTimeout time.Duration
	HAProxyNets    []*net.IPNet

	// XClient enables native inbound client-IP forwarding on this listener
	// (#742): IMAP ID fields, POP3/Submission XCLIENT. Mirrors the per-listener
	// xclient_protocol config key. Off = the forwarding commands are ignored
	// (ID replies NIL, XCLIENT is an unknown command).
	XClient bool
	// XClientNets are the CIDRs (general.xclient.trusted_nets) whose forwarded
	// client IP is trusted. A forwarded address is applied ONLY when the socket
	// peer — already PROXY-rewritten if HAProxy also ran — is inside one of
	// these ranges. Empty = trust nobody (every forward is ignored+logged).
	XClientNets []*net.IPNet
}

// liveSession tracks one active proxied session for kick support.
type liveSession struct {
	id          string
	user        string
	backendConn net.Conn
}

// watchConn wraps a proto.Conn for the persistent director watch connection.
// Writes are mutex-protected; reads happen in a dedicated goroutine.
//
// LOOKUP rides this same connection (#878). The director protocol echoes the
// request id in its HOST/FAIL reply, so the read loop can route replies to the
// caller that is waiting for them — which removes a dial (and, under internal
// TLS, a full handshake) from every login. Measured before this change:
// director_lookup cost 1.06s per login while the director's own
// yarilo_director_lookup_seconds reported 0.4ms of work.
type watchConn struct {
	mu sync.Mutex
	c  *proto.Conn

	pendMu  sync.Mutex
	pending map[string]chan string
}

// awaitReply registers id and returns the channel its reply will arrive on.
func (w *watchConn) awaitReply(id string) chan string {
	ch := make(chan string, 1)
	w.pendMu.Lock()
	if w.pending == nil {
		w.pending = make(map[string]chan string)
	}
	w.pending[id] = ch
	w.pendMu.Unlock()
	return ch
}

func (w *watchConn) forgetReply(id string) {
	w.pendMu.Lock()
	delete(w.pending, id)
	w.pendMu.Unlock()
}

// deliver routes a reply to its waiter. Reports false when nobody is waiting,
// which is how ordinary push lines fall through to the watch handling.
func (w *watchConn) deliver(id, line string) bool {
	w.pendMu.Lock()
	ch, ok := w.pending[id]
	if ok {
		delete(w.pending, id)
	}
	w.pendMu.Unlock()
	if !ok {
		return false
	}
	ch <- line
	return true
}

// failPending wakes every waiter when the connection dies, so a login in flight
// falls back to a fresh dial instead of waiting out its timeout.
func (w *watchConn) failPending() {
	w.pendMu.Lock()
	pending := w.pending
	w.pending = nil
	w.pendMu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

// lookup performs a LOOKUP over the persistent connection.
func (w *watchConn) lookup(id, username, tag, protoName string, timeout time.Duration) (proto.LookupResult, error) {
	ch := w.awaitReply(id)

	w.mu.Lock()
	err := w.c.WriteLine(proto.LookupRequestLine(id, username, tag, protoName))
	w.mu.Unlock()
	if err != nil {
		w.forgetReply(id)
		return proto.LookupResult{}, fmt.Errorf("director lookup: write: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line, ok := <-ch:
		if !ok {
			return proto.LookupResult{}, errWatchClosed
		}
		return proto.ParseLookupReply(line)
	case <-timer.C:
		w.forgetReply(id)
		return proto.LookupResult{}, fmt.Errorf("director lookup: timed out after %s", timeout)
	}
}

// errWatchClosed means the watch connection dropped while a lookup was in
// flight. The caller retries on a fresh connection: LOOKUP is a read of the
// routing decision, so repeating it is safe.
var errWatchClosed = errors.New("director watch connection closed")

// directorLookupTimeout bounds one LOOKUP over the persistent connection. Well
// above the director's own sub-millisecond handling, but low enough that a wedged
// connection falls back to a dial rather than holding the login.
const directorLookupTimeout = 10 * time.Second

func (w *watchConn) sessionOpen(sessID, username, backendIP, protoName string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine(fmt.Sprintf("SESSION-OPEN\t%s\t%s\t%s\t%s", sessID, proto.TabEscape(username), backendIP, protoName))
}

func (w *watchConn) sessionClose(sessID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine(fmt.Sprintf("SESSION-CLOSE\t%s", sessID))
}

func (w *watchConn) pong() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("PONG")
}

// sessionIDAlphabet is the 52-character set used by Postfix long queue IDs:
// digits 0-9, uppercase consonants B-Z, lowercase consonants b-z.
// Vowels (AEIOUaeiou) are excluded to avoid confusion when read aloud.
// 'z' (index 51) serves as the separator between the time and sequence
// parts; the sequence is encoded in the first 51 characters only so that
// 'z' never appears inside it, making the split unambiguous.
const sessionIDAlphabet = "0123456789BCDFGHJKLMNPQRSTVWXYZbcdfghjklmnpqrstvwxyz"

// encodeSessionPart encodes n in the given alphabet, left-padding with
// alphabet[0] to at least minLen characters.
func encodeSessionPart(n uint64, alphabet string, minLen int) string {
	base := uint64(len(alphabet))
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = alphabet[n%base]
		n /= base
	}
	for len(buf)-pos < minLen {
		pos--
		buf[pos] = alphabet[0]
	}
	return string(buf[pos:])
}

// Server is the login proxy server.
type Server struct {
	opts    Options
	reqID   atomic.Uint64
	sessSeq atomic.Uint64
	seed    string // 4 base51 chars, random per Server instance

	sessMu   sync.RWMutex
	sessions map[string][]*liveSession // username → active sessions

	watchMu sync.RWMutex
	watch   *watchConn // persistent director connection for push notifications

	// authMu guards the shared yarilo-auth client (#878). One multiplexed
	// connection serves every login on this pod: the AUTH wire protocol carries
	// a request id per command, so concurrent logins interleave over a single
	// mTLS session instead of each paying a fresh handshake. Created lazily
	// because the pod may start before yarilo-auth is reachable, and Serve runs
	// once per listener (imaps + imap) while the client must be a singleton.
	authMu sync.Mutex
	authCl *authclient.Client

	// anvilMu guards the shared yarilo-anvil pool (#878). Sessions no longer own
	// a connection: every anvil command carries the session id and the server
	// keeps no per-connection state, so a small fixed set of long-lived
	// connections serves every session on this pod. Lazy for the same reason as
	// the auth client — the pod may start before anvil is reachable.
	anvilMu   sync.Mutex
	anvilPool *anvil.Pool

	// Graceful-drain state (#857): on Shutdown the listeners are closed (stop
	// accepting) and inflight is waited on (let live sessions finish) up to the
	// grace period. draining makes a listener-closed Accept a clean return.
	drainMu   sync.Mutex
	listeners []net.Listener
	draining  bool
	inflight  sync.WaitGroup
}

// authClient returns the shared yarilo-auth client, dialling it on first use.
// A dial failure is returned to the caller (which surfaces the usual
// UNAVAILABLE) and leaves the field nil so the next login retries — the pod
// does not need auth to be up at start.
func (s *Server) authClient() (*authclient.Client, error) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authCl != nil {
		return s.authCl, nil
	}
	cl, err := authclient.Dial(s.opts.AuthAddr, s.opts.AuthTLS)
	if err != nil {
		return nil, err
	}
	s.authCl = cl
	return cl, nil
}

// anvilClient returns the shared anvil pool, creating it on first use. The pool
// dials lazily, so this cannot fail on an unreachable anvil — a dial error
// surfaces on the first command instead.
func (s *Server) anvilClient() *anvil.Pool {
	s.anvilMu.Lock()
	defer s.anvilMu.Unlock()
	if s.anvilPool == nil {
		s.anvilPool = anvil.NewPool(s.opts.AnvilAddr, s.opts.AnvilTLS, s.opts.AnvilConns, 0)
	}
	return s.anvilPool
}

func (s *Server) closeAnvilPool() {
	s.anvilMu.Lock()
	pool := s.anvilPool
	s.anvilPool = nil
	s.anvilMu.Unlock()
	if pool != nil {
		pool.Close()
	}
}

func (s *Server) closeAuthClient() {
	s.authMu.Lock()
	cl := s.authCl
	s.authCl = nil
	s.authMu.Unlock()
	if cl != nil {
		_ = cl.Close()
	}
}

// defaultTransientRetries is the extra-attempt budget for a transient failure.
// Three keeps a rolling restart of a dependency invisible to clients without
// holding a login long enough to look like a hang.
const defaultTransientRetries = 3

// transientRetryBackoff is the pause between attempts. Deliberately short: the
// failures this covers are a pod rolling or a connection being re-established,
// which resolve in well under a second.
const transientRetryBackoff = 150 * time.Millisecond

func (s *Server) transientRetries() int {
	if s.opts.TransientRetries > 0 {
		return s.opts.TransientRetries
	}
	if s.opts.TransientRetries < 0 {
		return 0 // explicit opt-out
	}
	return defaultTransientRetries
}

// newSessionID returns a Postfix-style long queue ID:
//
//	{base52(secs, ≥6)}{base52(usec, 4)}z{seed(4)}{base51(seq, ≥1)}
//
// The 4-char seed is random per Server instance (generated in New) so IDs
// are unique across pods even when secs+usec+seq coincide.
// Time parts use the full 52-char alphabet; seed and seq use the first 51
// chars so 'z' remains an unambiguous time/suffix separator.
func (s *Server) newSessionID() string {
	now := time.Now()
	secs := uint64(now.Unix())
	usec := uint64(now.Nanosecond() / 1000)
	seq := s.sessSeq.Add(1)
	return encodeSessionPart(secs, sessionIDAlphabet, 6) +
		encodeSessionPart(usec, sessionIDAlphabet, 4) +
		"z" +
		s.seed +
		encodeSessionPart(seq, sessionIDAlphabet[:51], 1)
}

// New creates a Server.
func New(opts Options) *Server {
	var b [3]byte
	_, _ = crand.Read(b[:])
	seed := uint64(b[0])<<16 | uint64(b[1])<<8 | uint64(b[2])
	return &Server{
		opts:     opts,
		seed:     encodeSessionPart(seed, sessionIDAlphabet[:51], 4),
		sessions: make(map[string][]*liveSession),
	}
}

// Serve accepts connections on ln until the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	if s.opts.HAProxy {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            haProxyPolicy(s.opts.HAProxyNets),
			ReadHeaderTimeout: timeout,
		}
	}
	s.drainMu.Lock()
	if s.draining {
		s.drainMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listeners = append(s.listeners, ln)
	s.drainMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startKickSubscriber(ctx)
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.drainMu.Lock()
			draining := s.draining
			s.drainMu.Unlock()
			if draining {
				return nil // listener closed by Shutdown — clean stop
			}
			return fmt.Errorf("login: accept: %w", err)
		}
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			s.handleConn(conn)
		}()
	}
}

// Shutdown stops accepting new connections (closes the listeners so Serve
// returns nil) and waits for in-flight proxied sessions to finish, up to ctx's
// deadline. A session still live when ctx expires is left to process exit —
// bounded by the pod terminationGracePeriodSeconds. Mirrors the director's
// graceful ring leave (#770). Safe to call once; idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	s.drainMu.Lock()
	if s.draining {
		s.drainMu.Unlock()
		return nil
	}
	s.draining = true
	lns := s.listeners
	s.listeners = nil
	s.drainMu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
		s.closeAuthClient()
		s.closeAnvilPool()
		return nil
	case <-ctx.Done():
		s.closeAuthClient()
		s.closeAnvilPool()
		return ctx.Err() // grace expired with sessions still live
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck

	remoteAddr := conn.RemoteAddr().String()
	clientIP, remotePort, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		clientIP = remoteAddr
		remotePort = ""
	}

	sessID := s.newSessionID()
	log := slog.With("sid", sessID, "proto", string(s.opts.Protocol), "remote_ip", clientIP, "remote_port", remotePort)
	log.Info("login: connect")

	// Implicit-TLS upgrade (IMAPS / POP3S / Submissions).
	if s.opts.ExtTLS != nil {
		tlsStart := time.Now()
		tlsConn := tls.Server(conn, s.opts.ExtTLS)
		if err := tlsConn.Handshake(); err != nil {
			log.Debug("login: tls handshake", "err", err)
			s.incResult("tls_error")
			return
		}
		s.observePhase(phaseTLSHandshake, tlsStart)
		conn = tlsConn
	}

	rd := bufio.NewReaderSize(conn, 4096)

	// Extract preamble: speak the protocol pre-auth exchange to collect credentials.
	// authConn/authRd may be TLS-upgraded from the original conn/rd if STARTTLS happened.
	preambleStart := time.Now()
	pre, authConn, authRd, err := extractPreamble(conn, rd, s.opts.Protocol, s.opts.StarttlsTLS, s.opts)
	if err != nil {
		log.Debug("login: preamble", "err", err)
		s.incResult("preamble_error")
		return
	}
	s.observePhase(phasePreamble, preambleStart)

	// Authenticate via yarilo-auth: passdb chain, brute-force penalty, token issuance.
	if s.opts.AuthAddr == "" {
		log.Error("login: auth_addr not configured")
		writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
		s.incResult("unavailable")
		return
	}
	// Shared multiplexed client (#878) — no per-login handshake. The phase
	// metric stays: it should now read ~0 except on the very first login after
	// a pod start or an auth reconnect, which is exactly the signal that the
	// reuse is working.
	authDialStart := time.Now()
	var authCl *authclient.Client
	for attempt := 0; ; attempt++ {
		var derr error
		authCl, derr = s.authClient()
		if derr == nil {
			break
		}
		if attempt >= s.transientRetries() {
			log.Error("login: yarilo-auth dial", "addr", s.opts.AuthAddr, "attempts", attempt+1, "err", derr)
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
			// Observed on failure too: a timed-out dial is the single most
			// important latency sample there is, and dropping it would make the
			// histogram look healthy exactly when the path is broken.
			s.observePhase(phaseAuthDial, authDialStart)
			s.incTransientExhausted(stageAuthDial)
			s.incResult("unavailable")
			return
		}
		log.Warn("login: yarilo-auth dial failed, retrying", "addr", s.opts.AuthAddr, "attempt", attempt+1, "err", derr)
		s.incTransientRetry(stageAuthDial)
		time.Sleep(transientRetryBackoff)
	}
	s.observePhase(phaseAuthDial, authDialStart)

	// Auth retry loop: keep the connection open after a bad-password failure.
	// Up to maxAuthAttempts attempts; after the last one send an untagged
	// BYE (IMAP) / -ERR (POP3) and close.
	maxAuthAttempts := s.opts.AuthMaxAttempts
	if maxAuthAttempts <= 0 {
		maxAuthAttempts = 3
	}
	var authResult *authclient.AuthResult
	for attempt := 1; ; attempt++ {
		// Native inbound client-IP forwarding (#742): the SINGLE point where a
		// proxy-forwarded address replaces the socket IP, so every downstream
		// consumer below (auth, allow_nets, anvil, the backend preamble ADDR=)
		// inherits it. pre.forwardIP is populated by the pre-auth parser ONLY
		// when this listener has xclient_protocol enabled; here we additionally
		// require the socket peer — already PROXY-rewritten if HAProxy also ran
		// — to be inside general.xclient.trusted_nets. Runs at the top of the
		// retry loop so a forward arriving in a retry iteration is honoured too.
		if pre.forwardIP != "" && clientIP != pre.forwardIP {
			if ipInNets(clientIP, s.opts.XClientNets) {
				log = log.With("orig_ip", clientIP, "fwd_ip", pre.forwardIP, "fwd_port", pre.forwardPort, "fwd_via", pre.forwardSource)
				log.Info("login: client ip forwarded")
				clientIP = pre.forwardIP
			} else if pre.forwardSource == "xclient" {
				// An untrusted peer sending XCLIENT is an anomaly — someone is
				// claiming to be a proxy.
				log.Warn("login: ignoring XCLIENT from untrusted peer", "peer_ip", clientIP, "claimed_ip", pre.forwardIP)
				pre.forwardIP = ""
			} else {
				// A bare IMAP ID with x-originating-ip is routine MUA chatter;
				// Debug, not Warn, to avoid log spam on every ordinary login.
				log.Debug("login: ignoring forwarded ID from untrusted peer", "peer_ip", clientIP, "claimed_ip", pre.forwardIP)
				pre.forwardIP = ""
			}
		}

		var aerr error
		// A temp-fail means "try again" by definition, so retry it here instead of
		// turning a passdb blip into a login the client can only recover from by
		// reconnecting (#896). Safe to repeat: yarilo-auth deliberately does NOT
		// touch the auth-penalty counter on internal failures, so a retry cannot
		// tarpit the user.
		for tfAttempt := 0; ; tfAttempt++ {
			authStart := time.Now()
			authResult, aerr = authCl.Authenticate(pre.username, pre.password, anvilService(s.opts.Protocol), clientIP, sessID)
			// One observation per attempt, not per login: the retry loop keeps the
			// connection open across a bad-password retry, and each attempt is its
			// own round-trip to yarilo-auth.
			s.observePhase(phaseAuth, authStart)
			if !errors.Is(aerr, authclient.ErrTempFail) {
				break
			}
			if tfAttempt >= s.transientRetries() {
				log.Warn("login: auth temp fail", "user", pre.username, "attempts", tfAttempt+1, "result", "fail")
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
				s.incTransientExhausted(stageAuth)
				s.incResult("unavailable")
				return
			}
			log.Warn("login: auth temp fail, retrying", "user", pre.username, "attempt", tfAttempt+1)
			s.incTransientRetry(stageAuth)
			time.Sleep(transientRetryBackoff)
		}

		authFailed := aerr != nil
		if aerr == nil {
			if authResult.Nologin {
				log.Info("login: auth", "user", pre.username, "result", "fail", "reason", "nologin", "attempt", attempt)
				authFailed = true
			} else if authResult.AllowNets != "" && !checkAllowNets(clientIP, authResult.AllowNets) {
				log.Info("login: auth", "user", pre.username, "result", "fail", "reason", "ip_not_in_allow_nets", "attempt", attempt)
				authFailed = true
			}
		} else {
			log.Info("login: auth", "user", pre.username, "result", "fail", "attempt", attempt)
		}

		if !authFailed {
			log.Info("login: auth", "user", pre.username, "result", "ok", "attempt", attempt)
			break
		}

		if attempt >= maxAuthAttempts || !isRetriableProtocol(s.opts.Protocol) {
			writeProtoError(authConn, s.opts.Protocol, "", imapCodeAuthenticationFail, "Too many failed authentications")
			return
		}
		writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeAuthenticationFail, "Authentication failed.")

		var retryExtTLS *tls.Config
		if _, ok := authConn.(*tls.Conn); !ok {
			retryExtTLS = s.opts.StarttlsTLS
		}
		pre, authConn, authRd, err = continueAuth(authConn, authRd, retryExtTLS, s.opts.Protocol, s.opts)
		if err != nil {
			log.Debug("login: preamble retry", "err", err)
			return
		}
		log.Info("login: auth retry", "user", pre.username, "attempt", attempt+1)
	}

	// Find backend address: fixed addr (standalone) or director LOOKUP (director mode).
	// tag is hoisted so the fast-fail re-route (#782) below can re-LOOKUP with it.
	var backendAddr, tag string
	if s.opts.BackendAddr != "" {
		backendAddr = s.opts.BackendAddr
	} else {
		// Per-user director_tag (#746, from the passdb/userdb response) wins
		// over the login component's static Tag config — lets a shared
		// login fleet route different users to different tag-pools.
		tag = s.opts.Tag
		if authResult.DirectorTag != "" {
			tag = authResult.DirectorTag
		}
		var err error
		lookupStart := time.Now()
		backendAddr, err = s.directorLookupWithHold(pre.username, tag, log)
		s.observePhase(phaseDirectorLookup, lookupStart)
		if err != nil {
			log.Warn("login: director lookup failed", "user", pre.username, "err", err)
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
			s.incResult("unavailable")
			return
		}
	}

	// Anvil connection limit check. Shared pool (#878): no per-session dial. The
	// phase metric now measures CONNECT over an already-open connection, so it
	// reads as a round trip rather than a full mTLS handshake.
	if s.opts.AnvilAddr != "" {
		anvilStart := time.Now()
		ap := s.anvilClient()
		svc := anvilService(s.opts.Protocol)
		cerr := ap.Connect(sessID, pre.username, clientIP, svc)
		s.observePhase(phaseAnvilConnect, anvilStart)
		switch {
		case errors.Is(cerr, anvil.ErrTooManyConns):
			log.Warn("login: anvil", "user", pre.username, "result", "fail", "reason", "too_many_connections")
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeLimit, "too many connections")
			return
		case cerr != nil:
			log.Error("login: anvil connect failed", "addr", s.opts.AnvilAddr, "err", cerr)
			if !s.opts.AnvilFailOpen {
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
				s.incResult("unavailable")
				return
			}
		default:
			log.Info("login: anvil", "user", pre.username, "result", "ok")
			// #814: record the routed backend in anvil so `who` can scope to the
			// local backend. Best-effort.
			if beIP, _, splitErr := net.SplitHostPort(backendAddr); splitErr == nil {
				if berr := ap.Backend(sessID, beIP); berr != nil {
					log.Debug("login: anvil backend push", "err", berr)
				}
			}
			hbCtx, hbCancel := context.WithCancel(context.Background())
			hbDone := make(chan struct{})
			go func() {
				defer close(hbDone)
				interval := anvil.DefaultSessionTTL / 3
				if err := ap.HeartbeatLoop(hbCtx, sessID, interval, nil); err != nil {
					log.Debug("login: anvil heartbeat loop", "err", err)
				}
			}()
			// The pool outlives the session, so there is nothing to close here —
			// only the registration to release.
			defer func() {
				hbCancel()
				<-hbDone
				if err := ap.Disconnect(sessID, pre.username, clientIP, svc); err != nil {
					log.Debug("login: anvil disconnect", "err", err)
				}
			}()
		}
	}

	// Bring up the backend session, retrying transient failures (#896). Dial,
	// preamble and greeting are retried as one unit because a failed greeting
	// leaves the connection unusable — the whole bring-up has to be redone, and
	// dialBackendWithReroute may land on a different backend next time.
	backendDialStart := time.Now()
	var bs *backendSession
	retries := s.transientRetries()
	for attempt := 0; ; attempt++ {
		var berr error
		bs, berr = s.openBackendSession(pre, authResult, tag, backendAddr, clientIP, sessID, log)
		if berr == nil {
			break
		}
		if attempt >= retries {
			log.Error("login: backend session failed", "addr", backendAddr, "attempts", attempt+1, "err", berr)
			s.observePhase(phaseBackendDial, backendDialStart)
			s.incTransientExhausted(stageBackendSession)
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
			s.incResult("unavailable")
			return
		}
		log.Warn("login: backend session failed, retrying", "addr", backendAddr, "attempt", attempt+1, "err", berr)
		s.incTransientRetry(stageBackendSession)
		time.Sleep(transientRetryBackoff)
	}
	s.observePhase(phaseBackendDial, backendDialStart)

	backendConn, backendRd, backendAddr, backendCaps := bs.conn, bs.rd, bs.addr, bs.caps
	defer backendConn.Close()

	// Register the session for kick support only once it is actually up — a
	// bring-up that never completed has nothing to kick.
	sess := &liveSession{id: sessID, user: pre.username, backendConn: backendConn}
	s.sessMu.Lock()
	s.sessions[pre.username] = append(s.sessions[pre.username], sess)
	s.sessMu.Unlock()
	defer func() {
		s.sessMu.Lock()
		list := s.sessions[pre.username]
		for i, v := range list {
			if v == sess {
				s.sessions[pre.username] = append(list[:i], list[i+1:]...)
				break
			}
		}
		s.sessMu.Unlock()
	}()

	backendIP, _, _ := net.SplitHostPort(backendAddr)
	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc != nil {
		wc.sessionOpen(sessID, pre.username, backendIP, s.opts.Protocol.Base())
		defer wc.sessionClose(sessID)
	}

	log.Info("login: session routed", "user", pre.username, "backend", backendAddr, "result", "ok")
	s.incResult("ok")

	// Auth is confirmed — tell the client before entering proxy mode.
	writeProtoAuthOK(authConn, s.opts.Protocol, pre.cmdTag, backendCaps)

	authConn.SetDeadline(time.Time{})    //nolint:errcheck
	backendConn.SetDeadline(time.Time{}) //nolint:errcheck

	proto := string(s.opts.Protocol)
	sessionsGauge.WithLabelValues(proto).Inc()
	biProxy(authRd, authConn, backendRd, backendConn)
	sessionsGauge.WithLabelValues(proto).Dec()
	log.Info("login: disconnect", "user", pre.username)
}

// directorLookup dials yarilo-director, issues a LOOKUP restricted to tag,
// and returns the backend address.
func (s *Server) directorLookup(username, tag string) (string, error) {
	id := fmt.Sprintf("%d", s.reqID.Add(1))

	// Prefer the persistent watch connection (#878). It is absent only before the
	// watch has connected or between reconnects, in which case fall through to a
	// dial so a login is never blocked on the watch being up.
	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc != nil {
		result, lerr := wc.lookup(id, username, tag, s.opts.Protocol.Base(), directorLookupTimeout)
		if lerr == nil {
			return s.applyBackendPort(result.Addr), nil
		}
		// A dead connection or a hold is not a reason to skip the fallback path
		// for the former; a hold must propagate unchanged so the caller retries.
		if !errors.Is(lerr, errWatchClosed) {
			return "", lerr
		}
	}

	return s.directorLookupDial(id, username, tag)
}

// applyBackendPort overrides the ring port the director returns with the
// component's configured backend port.
func (s *Server) applyBackendPort(addr string) string {
	if s.opts.BackendPort <= 0 {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", s.opts.BackendPort))
}

// directorLookupDial is the fallback: one connection for one lookup.
func (s *Server) directorLookupDial(id, username, tag string) (string, error) {
	var c *proto.Conn
	var err error
	if s.opts.DirectorTLS != nil {
		c, err = proto.DialTLS(s.opts.DirectorAddr, s.opts.LocalIP, 0, s.opts.DirectorTLS)
	} else {
		c, err = proto.Dial(s.opts.DirectorAddr, s.opts.LocalIP, 0)
	}
	if err != nil {
		return "", fmt.Errorf("director dial: %w", err)
	}
	defer c.Close()

	result, err := c.Lookup(id, username, tag, s.opts.Protocol.Base())
	if err != nil {
		return "", fmt.Errorf("director lookup: %w", err)
	}
	return s.applyBackendPort(result.Addr), nil
}

// defaultMaxLookupHolds / defaultLookupHoldBackoff bound the confirmed-kick
// retry (#847): a LOOKUP held while the user's old sessions drain is re-tried
// rather than surfaced as a client error. The total budget
// (holds × backoff = 3s) MUST exceed the director's worst-case confirm time
// (user_kill_confirm_grace + drain) or the proxy exhausts its retries before the
// kill confirms and errors the concurrent login (#858). Overridable via
// login.lookup_hold_max / lookup_hold_backoff_ms so an operator who raises the
// director's confirm grace can raise the budget alongside it. A kill that still
// outlasts the budget means the director's hard timeout has cleared the hold, so
// the next fresh login succeeds.
const (
	defaultMaxLookupHolds    = 20
	defaultLookupHoldBackoff = 150 * time.Millisecond
)

func (s *Server) maxLookupHolds() int {
	if s.opts.LookupHoldMax > 0 {
		return s.opts.LookupHoldMax
	}
	return defaultMaxLookupHolds
}

func (s *Server) lookupHoldBackoff() time.Duration {
	if s.opts.LookupHoldBackoff > 0 {
		return s.opts.LookupHoldBackoff
	}
	return defaultLookupHoldBackoff
}

// directorLookupWithHold performs a director LOOKUP, retrying on a retryable
// confirmed-kick hold (#847, proto.ErrLookupHold) with a bounded backoff. Any
// other error (or success) returns immediately.
func (s *Server) directorLookupWithHold(username, tag string, log *slog.Logger) (string, error) {
	maxHolds, backoff := s.maxLookupHolds(), s.lookupHoldBackoff()
	for attempt := 0; ; attempt++ {
		addr, err := s.directorLookup(username, tag)
		if err == nil || !errors.Is(err, proto.ErrLookupHold) {
			return addr, err
		}
		if attempt >= maxHolds {
			return "", err
		}
		log.Debug("login: director holding lookup (user kill in progress), retrying", "user", username, "attempt", attempt+1)
		time.Sleep(backoff)
	}
}

// maxBackendReroutes bounds the active fast-fail re-route (#782): after the
// first dial fails we re-LOOKUP at most this many times. Kept small on purpose
// — the re-route is an accelerator over the TTL/corroboration path, not a
// retry storm; a re-LOOKUP that returns the SAME (still-dead) pod stops it
// early regardless.
const maxBackendReroutes = 1

// backendSession is a backend connection that completed the preamble handshake
// and, for submission, its EHLO exchange.
type backendSession struct {
	conn net.Conn
	rd   *bufio.Reader
	addr string
	caps string
}

// openBackendSession dials a backend and brings the session up to the point
// where the client can be told the login succeeded. Every failure closes the
// connection before returning, so the caller may simply retry (#896).
func (s *Server) openBackendSession(pre *preamble, authResult *authclient.AuthResult, tag, addr, clientIP, sessID string, log *slog.Logger) (*backendSession, error) {
	// Fast-fail re-route on a connect failure in director mode (#782): report the
	// backend unreachable and re-LOOKUP.
	conn, addr, err := s.dialBackendWithReroute(pre.username, tag, addr, log)
	if err != nil {
		return nil, fmt.Errorf("dial backend %s: %w", addr, err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	rd := bufio.NewReaderSize(conn, 4096)

	// The backend's PreambleListener reads this line and calls yarilo-auth VERIFY.
	pre2 := loginproto.Preamble{
		Addr:      clientIP,
		SessionID: sessID,
		User:      pre.username,
		Token:     authResult.Token,
		Helo:      pre.ehloLine,
	}
	if _, werr := io.WriteString(conn, pre2.Format()); werr != nil {
		return nil, fmt.Errorf("send preamble: %w", werr)
	}

	// Read the backend greeting; for IMAP this carries the post-auth capabilities
	// echoed in the tagged OK. A backend that closes here (token VERIFY failed,
	// or it is shutting down) must be reported, not silently dropped.
	greetingStart := time.Now()
	caps, gerr := readBackendGreeting(rd, s.opts.Protocol)
	s.observePhase(phaseBackendPreamble, greetingStart)
	if gerr != nil {
		s.incResult("backend_rejected")
		return nil, fmt.Errorf("backend rejected session: %w", gerr)
	}

	// SMTP submission: send EHLO so the backend has a HELO domain before the
	// client sends MAIL FROM through the proxy.
	if isSubmission(s.opts.Protocol) {
		ehlo := pre.ehloLine
		if ehlo == "" {
			ehlo = "EHLO yarilo-submission-login\r\n"
		}
		if _, werr := io.WriteString(conn, ehlo); werr != nil {
			return nil, fmt.Errorf("smtp ehlo send: %w", werr)
		}
		for {
			line, rerr := rd.ReadString('\n')
			if rerr != nil {
				return nil, fmt.Errorf("smtp ehlo response: %w", rerr)
			}
			if len(line) >= 4 && line[3] != '-' {
				break
			}
		}
	}

	ok = true
	return &backendSession{conn: conn, rd: rd, addr: addr, caps: caps}, nil
}

// dialBackendWithReroute dials addr and, on a connect failure in director mode,
// performs the login-proxy half of the active fast-fail re-route (#782): it
// reports the backend unreachable to the director (which corroborates across
// proxies and evicts early) and re-LOOKUPs for a live pod. It gives up — rather
// than spin — when the re-LOOKUP returns the SAME address, because that means
// the ring has not dropped the dead pod yet (below the corroboration threshold,
// or this proxy is the first reporter); the client then gets a transient
// unavailable and reconnects, by which point corroboration or the TTL lease has
// rehashed it. Returns the working conn and the address actually connected to.
func (s *Server) dialBackendWithReroute(username, tag, addr string, log *slog.Logger) (net.Conn, string, error) {
	conn, err := dialBackend(addr, s.opts.BackendTLS)
	if err == nil {
		return conn, addr, nil
	}
	// Standalone mode (fixed backend) has no director to re-route through.
	if s.opts.BackendAddr != "" {
		return nil, addr, err
	}
	for attempt := 0; attempt < maxBackendReroutes; attempt++ {
		log.Warn("login: backend dial failed — reporting unreachable and re-looking-up", "addr", addr, "err", err)
		s.reportUnreachable(addr)
		newAddr, lerr := s.directorLookup(username, tag)
		if lerr != nil {
			return nil, addr, fmt.Errorf("re-lookup after unreachable: %w", lerr)
		}
		if newAddr == addr {
			return nil, addr, fmt.Errorf("re-lookup returned the same unreachable backend %s", addr)
		}
		addr = newAddr
		conn, err = dialBackend(addr, s.opts.BackendTLS)
		if err == nil {
			return conn, addr, nil
		}
	}
	return nil, addr, fmt.Errorf("backend unreachable after re-route: %w", err)
}

// reportUnreachable tells the director a dial to backendAddr failed (#782).
// Best-effort: a fresh short-lived director connection, errors only logged —
// the report is an accelerator, and the TTL lease remains the backstop.
func (s *Server) reportUnreachable(backendAddr string) {
	ip, _, err := net.SplitHostPort(backendAddr)
	if err != nil {
		ip = backendAddr
	}
	var c *proto.Conn
	if s.opts.DirectorTLS != nil {
		c, err = proto.DialTLS(s.opts.DirectorAddr, s.opts.LocalIP, 0, s.opts.DirectorTLS)
	} else {
		c, err = proto.Dial(s.opts.DirectorAddr, s.opts.LocalIP, 0)
	}
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.Unreachable(ip)
}

// Watch maintains a persistent director connection for receiving USER-KICKED
// pushes (#736) — the push plane that makes admin/backend-down kicks actually
// reach this login pod's sessions. Start it as a goroutine per Server before
// serving. No-op without a director (standalone / BackendAddr mode).
func (s *Server) Watch(ctx context.Context) {
	if s.opts.DirectorAddr == "" {
		return
	}
	backoff := 2 * time.Second
	for {
		s.runWatch(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("login: director watch disconnected, reconnecting", "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

func (s *Server) runWatch(ctx context.Context) {
	var c *proto.Conn
	var err error
	if s.opts.DirectorTLS != nil {
		c, err = proto.DialTLS(s.opts.DirectorAddr, s.opts.LocalIP, 0, s.opts.DirectorTLS)
	} else {
		c, err = proto.Dial(s.opts.DirectorAddr, s.opts.LocalIP, 0)
	}
	if err != nil {
		slog.Warn("login: director watch dial failed", "err", err)
		return
	}
	defer c.Close()

	wc := &watchConn{c: c}
	s.watchMu.Lock()
	s.watch = wc
	s.watchMu.Unlock()
	defer func() {
		s.watchMu.Lock()
		if s.watch == wc {
			s.watch = nil
		}
		s.watchMu.Unlock()
	}()

	slog.Info("login: director watch connected", "addr", s.opts.DirectorAddr)

	readErr := make(chan error, 1)
	go func() {
		err := s.watchReadLoop(c, wc)
		wc.failPending()
		readErr <- err
	}()

	select {
	case <-ctx.Done():
		c.Close()
		<-readErr
	case err := <-readErr:
		if err != nil {
			slog.Warn("login: director watch read error", "err", err)
		}
	}
}

func (s *Server) watchReadLoop(c *proto.Conn, wc *watchConn) error {
	for {
		line, err := c.ReadLine()
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(line, "HOST\t"), strings.HasPrefix(line, "FAIL\t"):
			// Reply to a LOOKUP issued on this connection; route it by id. An
			// unclaimed id is a reply nobody waits for any more (its caller timed
			// out) and is dropped.
			if fields := strings.Split(line, "\t"); len(fields) >= 2 {
				wc.deliver(fields[1], line)
			}
		case strings.HasPrefix(line, "USER-KICKED\t"):
			fields := strings.SplitN(line, "\t", 2)
			if len(fields) == 2 {
				s.kickUser(fields[1])
			}
		case line == "PING":
			wc.pong()
		}
		// OK and other push lines silently ignored.
	}
}

// kickUser closes all active backend connections for the given username,
// causing biProxy to terminate and those sessions to be dropped.
func (s *Server) kickUser(username string) {
	s.sessMu.RLock()
	sessions := make([]*liveSession, len(s.sessions[username]))
	copy(sessions, s.sessions[username])
	s.sessMu.RUnlock()

	for _, sess := range sessions {
		slog.Info("login: kicking session", "user", username, "session", sess.id)
		sess.backendConn.Close()
	}
}

// kickSession closes the backend connection of the session with
// the given id, regardless of which user owns it. Returns true
// when a matching session was found and closed. Silently no-ops
// when nothing matches — kick events are broadcast to every pod
// and only the owner reacts.
func (s *Server) kickSession(id string) bool {
	s.sessMu.RLock()
	var target *liveSession
	var user string
findLoop:
	for u, list := range s.sessions {
		for _, sess := range list {
			if sess.id == id {
				target = sess
				user = u
				break findLoop
			}
		}
	}
	s.sessMu.RUnlock()
	if target == nil {
		return false
	}
	slog.Info("login: kicking session by id", "user", user, "session", id)
	target.backendConn.Close()
	return true
}

// kickChannel is the anvil pub/sub channel this login pod
// subscribes to. Keyed per-protocol so each login binary only
// wakes up for relevant events. Event payload is the session id.
func (s *Server) kickChannel() string {
	return "kick:" + string(s.opts.Protocol)
}

// startKickSubscriber dials anvil on a dedicated connection and
// drives kickSession from incoming EVENT lines for the
// per-protocol kick channel. No-op when AnvilAddr is unset
// (single-process dev runs). The goroutine exits when ctx is
// cancelled or the underlying conn errors out — Serve restart
// re-subscribes.
func (s *Server) startKickSubscriber(ctx context.Context) {
	if s.opts.AnvilAddr == "" {
		return
	}
	channel := s.kickChannel()
	attempts := s.opts.DialRetries
	if attempts <= 0 {
		attempts = 1
	}
	var ac *anvil.Conn
	err := retry.Do(ctx, attempts, time.Second, func() error {
		var e error
		ac, e = anvil.Dial(s.opts.AnvilAddr, s.opts.AnvilTLS, 5*time.Second)
		return e
	})
	if err != nil {
		slog.Error("login: kick subscribe dial failed", "addr", s.opts.AnvilAddr, "attempts", attempts, "err", err)
		return
	}
	ch, err := ac.Subscribe(ctx, channel)
	if err != nil {
		ac.Close()
		slog.Error("login: kick subscribe failed", "channel", channel, "err", err)
		return
	}
	go func() {
		defer ac.Close()
		for sessID := range ch {
			if !s.kickSession(sessID) {
				slog.Debug("login: kick event ignored (no match)", "session", sessID)
			}
		}
	}()
}

func dialBackend(addr string, tlsCfg *tls.Config) (net.Conn, error) {
	if tlsCfg != nil {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("mtls dial %s: %w", addr, err)
		}
		return conn, nil
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// readBackendGreeting reads the backend's greeting after the preamble is
// processed. For IMAP it extracts and returns the post-auth capability list
// from the "* PREAUTH [CAPABILITY ...]" line so the login pod can include it
// verbatim in the tagged OK response sent to the client.
func readBackendGreeting(rd *bufio.Reader, p Protocol) (caps string, err error) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		line, err := rd.ReadString('\n')
		if err != nil {
			return "", err
		}
		// Extract content of [CAPABILITY ...] if present.
		if start := strings.Index(line, "[CAPABILITY "); start >= 0 {
			start += len("[CAPABILITY ")
			if end := strings.Index(line[start:], "]"); end >= 0 {
				caps = line[start : start+end]
			}
		}
		return caps, nil
	case ProtocolPOP3, ProtocolPOP3S:
		_, err := rd.ReadString('\n')
		return "", err
	case ProtocolSubmission, ProtocolSubmissions:
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return "", err
			}
			if len(line) < 4 || line[3] != '-' {
				return "", nil
			}
		}
	case ProtocolManageSieve:
		// Consume the backend's pre-auth greeting (capability lines + OK).
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(line, "OK") {
				return "", nil
			}
		}
	}
	return "", nil
}

// checkAllowNets reports whether clientIP is contained in any of the comma-separated
// CIDR ranges from the allow_nets= field returned by yarilo-auth.
func checkAllowNets(clientIP, allowNets string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	for _, cidr := range strings.Split(allowNets, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// writeProtoAuthOK sends the protocol-specific authentication-success response to
// the client. caps is the post-auth IMAP capability list extracted from the
// backend PREAUTH greeting; included as [CAPABILITY ...] in the tagged OK when
// non-empty so clients skip a separate CAPABILITY round-trip.
func writeProtoAuthOK(conn net.Conn, p Protocol, tag, caps string) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		if caps != "" {
			fmt.Fprintf(conn, "%s OK [CAPABILITY %s] Logged in\r\n", tag, caps) //nolint:errcheck
		} else {
			fmt.Fprintf(conn, "%s OK Logged in\r\n", tag) //nolint:errcheck
		}
	case ProtocolPOP3, ProtocolPOP3S:
		fmt.Fprintf(conn, "+OK Logged in\r\n") //nolint:errcheck
	case ProtocolSubmission, ProtocolSubmissions:
		fmt.Fprintf(conn, "235 2.7.0 Authentication successful\r\n") //nolint:errcheck
	case ProtocolManageSieve:
		fmt.Fprintf(conn, "OK \"Logged in.\"\r\n") //nolint:errcheck
	}
}

// biProxy copies data bidirectionally until either side closes.
func biProxy(clientRd io.Reader, clientW io.Writer, backendRd io.Reader, backendW io.Writer) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(backendW, clientRd) //nolint:errcheck
		halfClose(backendW)
	}()
	go func() {
		defer wg.Done()
		io.Copy(clientW, backendRd) //nolint:errcheck
		halfClose(clientW)
	}()
	wg.Wait()
}

func halfClose(w io.Writer) {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := w.(halfCloser); ok {
		hc.CloseWrite() //nolint:errcheck
	}
}

// IMAP response codes (RFC 5530) used in NO responses.
const (
	imapCodeUnavailable        = "UNAVAILABLE"
	imapCodeAuthenticationFail = "AUTHENTICATIONFAILED"
	imapCodeLimit              = "LIMIT"
)

func writeProtoError(conn net.Conn, p Protocol, tag, imapCode, msg string) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		if tag != "" {
			fmt.Fprintf(conn, "%s NO [%s] %s\r\n", tag, imapCode, msg) //nolint:errcheck
		} else {
			fmt.Fprintf(conn, "* BYE %s\r\n", msg) //nolint:errcheck
		}
	case ProtocolPOP3, ProtocolPOP3S:
		// RFC 3206: map error class to [AUTH] / [SYS/TEMP] response codes.
		switch imapCode {
		case imapCodeAuthenticationFail:
			fmt.Fprintf(conn, "-ERR [AUTH] %s\r\n", msg) //nolint:errcheck
		case imapCodeUnavailable:
			fmt.Fprintf(conn, "-ERR [SYS/TEMP] %s\r\n", msg) //nolint:errcheck
		default:
			fmt.Fprintf(conn, "-ERR %s\r\n", msg) //nolint:errcheck
		}
	case ProtocolSubmission, ProtocolSubmissions:
		fmt.Fprintf(conn, "421 4.3.0 %s\r\n", msg) //nolint:errcheck
	case ProtocolManageSieve:
		switch imapCode {
		case imapCodeAuthenticationFail:
			fmt.Fprintf(conn, "NO (AUTHENTICATIONFAILED) %q\r\n", msg) //nolint:errcheck
		default:
			fmt.Fprintf(conn, "BYE %q\r\n", msg) //nolint:errcheck
		}
	}
}

func isSubmission(p Protocol) bool {
	return p == ProtocolSubmission || p == ProtocolSubmissions
}

// isRetriableProtocol reports whether the protocol keeps the connection open
// after a failed authentication attempt (IMAP and POP3 do; SMTP closes).
func isRetriableProtocol(p Protocol) bool {
	return p == ProtocolIMAP || p == ProtocolIMAPS ||
		p == ProtocolPOP3 || p == ProtocolPOP3S ||
		p == ProtocolManageSieve
}

// anvilService maps a login Protocol to the service name used in the anvil protocol.
func anvilService(p Protocol) string {
	switch p {
	case ProtocolPOP3, ProtocolPOP3S:
		return "pop3"
	case ProtocolSubmission, ProtocolSubmissions:
		return "smtp"
	case ProtocolManageSieve:
		return "managesieve"
	default:
		return "imap"
	}
}

// ipInNets reports whether the string IP is inside one of the CIDRs. Used to
// gate native XCLIENT/ID forwarding on general.xclient.trusted_nets (#742).
// Empty nets = trust nobody.
func ipInNets(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

func haProxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcpAddr, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcpAddr.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}
