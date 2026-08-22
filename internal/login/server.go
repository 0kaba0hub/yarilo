// Package login implements the mail-protocol login proxy: it authenticates the
// client, resolves the backend via director LOOKUP, and proxies the session.
// TLS terminates here; backends receive plain TCP (or mTLS internally).
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

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/cluster/proto"
	"github.com/yarilomail/yarilo/internal/loginproto"
	"github.com/yarilomail/yarilo/internal/warden"
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

// Base collapses a listener protocol to its backend container name
// (imaps→imap, pop3s→pop3, submissions→submission) — the granularity the
// director counts sessions at. Sent as the trailing proto field on
// LOOKUP / SESSION-OPEN.
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
	// Tag restricts director LOOKUP to backends with this tag.
	// "" means the untagged pool, not "any tag".
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
	// WardenAddr is the host:port of yarilo-warden for per-user@IP connection
	// limiting (mail_max_userip_connections). Empty = no limit enforcement.
	WardenAddr string
	// WardenTLS is the mTLS config for connecting to yarilo-warden.
	// Nil means plain TCP.
	WardenTLS *tls.Config
	// WardenFailOpen controls what happens when yarilo-warden is unreachable.
	// true = allow the session (fail open); false = reject the session (fail closed).
	WardenFailOpen bool
	// TransientRetries is the extra-attempt budget for transient failures
	// (auth temp-fail, auth dial, backend bring-up). 0 selects the default (3).
	TransientRetries int
	// TransientReloginCap is how many transient failures one connection may
	// answer with a tagged NO before it is closed. cap=N permits N tagged NOs.
	// Independent of AuthMaxAttempts. 0 selects the default (3).
	TransientReloginCap int
	// WardenConns is the size of the shared warden connection pool.
	// 0 selects warden.DefaultPoolSize. The warden protocol has no request id,
	// so a connection serves one command at a time.
	WardenConns int
	// DialRetries is the number of attempts (with exponential backoff) when
	// dialling external dependencies at startup. 0 or 1 means a single attempt.
	DialRetries int

	// LookupHoldMax / LookupHoldBackoff bound the confirmed-kick LOOKUP retry.
	// Their product must exceed the director's worst-case confirm time.
	// 0 uses the defaults (20 / 150ms → 3s budget). From
	// login.lookup_hold_max / lookup_hold_backoff_ms.
	// SessionSyncInterval paces the full session list this pod sends the
	// director. Zero selects the default; negative sends it only when a watch
	// connection is (re)established. See syncSessions (#1393).
	SessionSyncInterval time.Duration
	LookupHoldMax       int
	LookupHoldBackoff   time.Duration

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

	// XClient enables inbound client-IP forwarding on this listener
	// (IMAP ID fields, POP3/Submission XCLIENT). Mirrors xclient_protocol.
	// Off = ID replies NIL, XCLIENT is an unknown command.
	XClient bool
	// XClientNets (general.xclient.trusted_nets) are the CIDRs whose forwarded
	// client IP is trusted; the socket peer must be inside one of them.
	// Empty = trust nobody.
	XClientNets []*net.IPNet
}

// liveSession tracks one active proxied session for kick support.
type liveSession struct {
	id          string
	user        string
	backendConn net.Conn
	// clientConn is the other leg. A kick must close BOTH: biProxy waits for
	// its two copies, and the client-to-backend one sits on a client that may
	// say nothing for minutes, so closing the backend alone leaves the proxy
	// running -- and with it the session record and the SESSION-CLOSE the
	// director waits for to confirm the kill (#1366).
	clientConn net.Conn
	// backendIP and proto are what the director was told about this session.
	// Kept so the session can be announced AGAIN: the announcement is made to
	// one director, and when that director dies the session outlives its
	// record (#1393).
	backendIP string
	proto     string
}

// watchConn wraps a proto.Conn for the persistent director watch connection.
// Writes are mutex-protected; reads happen in a dedicated goroutine.
//
// LOOKUP rides this same connection: the director echoes the request id in its
// HOST/FAIL reply, so the read loop routes replies to waiting callers. This
// removes a dial (and TLS handshake) from every login.
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

// deliver routes a reply to its waiter. Reports false when nobody is waiting;
// ordinary push lines fall through to the watch handling.
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

// failPending wakes every waiter when the connection dies; an in-flight login
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
// flight. Safe to retry on a fresh connection: LOOKUP is a read.
var errWatchClosed = errors.New("director watch connection closed")

// directorLookupTimeout bounds one LOOKUP over the persistent connection;
// a wedged connection falls back to a dial rather than holding the login.
const directorLookupTimeout = 10 * time.Second

func (w *watchConn) sessionOpen(sessID, username, backendIP, protoName string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine(fmt.Sprintf("SESSION-OPEN\t%s\t%s\t%s\t%s", sessID, proto.TabEscape(username), backendIP, protoName))
}

// The reconciliation is framed START ... chunks ... END so the director can
// tell a complete list from a truncated one: applying half a list would erase
// live sessions. The framing follows the handshake's HOST-HAND-START/END.
func (w *watchConn) sessionSyncStart() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("SESSION-SYNC-START")
}

func (w *watchConn) sessionSyncChunk(ids []string) {
	if len(ids) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("SESSION-SYNC\t" + strings.Join(ids, "\t"))
}

func (w *watchConn) sessionSyncEnd() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("SESSION-SYNC-END")
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

// sessionIDAlphabet is the 52-character Postfix long-queue-ID set: digits,
// consonants B-Z/b-z (vowels excluded). 'z' (index 51) separates the time and
// sequence parts; the sequence uses only the first 51 characters so 'z' never
// appears inside it.
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

	sessMu sync.RWMutex
	// announceMu orders what this pod tells the director about its sessions.
	// A session registers and announces itself under it, and the full-list
	// reconciliation takes it too -- so a list can never be built before a
	// session exists and sent after that session's SESSION-OPEN (#1393).
	announceMu sync.Mutex
	sessions   map[string][]*liveSession // username → active sessions

	watchMu sync.RWMutex
	watch   *watchConn // persistent director connection for push notifications

	// Shared yarilo-auth client. The AUTH wire protocol carries a request id
	// per command, so concurrent logins multiplex over one connection.
	// Created lazily: the pod may start before yarilo-auth is reachable.
	authMu sync.Mutex
	authCl *authclient.Client

	// Shared yarilo-warden pool. Every warden command carries the session id,
	// so a small fixed pool serves every session. Lazy for the same reason as
	// the auth client.
	wardenMu   sync.Mutex
	wardenPool *warden.Pool

	// Graceful-drain state: Shutdown closes the listeners and waits on inflight
	// up to the grace period. draining makes a listener-closed Accept a clean
	// return.
	drainMu   sync.Mutex
	listeners []net.Listener
	draining  bool
	inflight  sync.WaitGroup
}

// authClient returns the shared yarilo-auth client, dialling it on first use.
// A dial failure leaves the field nil so the next login retries.
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

// wardenClient returns the shared warden pool, creating it on first use.
// The pool dials lazily; a dial error surfaces on the first command.
func (s *Server) wardenClient() *warden.Pool {
	s.wardenMu.Lock()
	defer s.wardenMu.Unlock()
	if s.wardenPool == nil {
		s.wardenPool = warden.NewPool(s.opts.WardenAddr, s.opts.WardenTLS, s.opts.WardenConns, 0)
	}
	return s.wardenPool
}

func (s *Server) closeWardenPool() {
	s.wardenMu.Lock()
	pool := s.wardenPool
	s.wardenPool = nil
	s.wardenMu.Unlock()
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
const defaultTransientRetries = 3

// transientRetryBackoff is the pause between attempts. Short: the failures it
// covers (pod rolling, connection re-established) resolve in under a second.
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

// defaultTransientReloginCap is the client-side re-LOGIN budget per connection.
// Distinct from transientRetries, the per-hop internal budget.
const defaultTransientReloginCap = 3

func (s *Server) transientReloginCap() int {
	if s.opts.TransientReloginCap > 0 {
		return s.opts.TransientReloginCap
	}
	if s.opts.TransientReloginCap < 0 {
		return 0 // explicit opt-out: close on the first transient
	}
	return defaultTransientReloginCap
}

// newSessionID returns a Postfix-style long queue ID:
//
//	{base52(secs, ≥6)}{base52(usec, 4)}z{seed(4)}{base51(seq, ≥1)}
//
// The 4-char seed is random per Server instance so IDs are unique across pods.
// Time parts use the full 52-char alphabet; seed and seq use the first 51
// chars so 'z' remains an unambiguous separator.
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

// Shutdown stops accepting new connections and waits for in-flight sessions
// up to ctx's deadline. Sessions still live at expiry are left to process exit
// (bounded by terminationGracePeriodSeconds). Idempotent.
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
		s.closeWardenPool()
		return nil
	case <-ctx.Done():
		s.closeAuthClient()
		s.closeWardenPool()
		return ctx.Err() // grace expired with sessions still live
	}
}

// loginOutcome is the result of one login pass: proxy, close, or keep the
// connection open for the client to retry.
type loginOutcome int

const (
	outcomeDone  loginOutcome = iota // authenticated and brought up; proxy it
	outcomeClose                     // fatal (bad-password exhausted, over limit); close
	outcomeRetry                     // transient; a tagged NO is sent, keep the connection for a re-LOGIN
)

// established is what a successful login pass hands back to handleConn to
// proxy and own for the session's lifetime.
type established struct {
	bs *backendSession
	// user is the identity this session acts as -- what the auth service
	// resolved, which for a master login is the target rather than the string
	// the client typed. Everything keyed by identity after bring-up (the kick
	// registry, the session watch) reads it from here, so the resolution made
	// once at authentication cannot be re-derived differently later (#1306).
	user          string
	releaseWarden func() // warden heartbeat-cancel + Disconnect; nil when warden is disabled
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

	// attempt runs one authenticate→route→bring-up pass. On a transient failure
	// it returns outcomeRetry, having written a tagged NO [UNAVAILABLE] and
	// released anything it acquired, so the client can LOGIN again on the same
	// connection. pre/authConn/authRd are the outer per-connection state; the
	// bad-password sub-loop mutates them in place.
	var est *established
	attempt := func() (loginOutcome, *established) {
		// committed flips only on a successful bring-up; until then the deferred
		// unwind releases the warden slot. On success the release is handed to
		// handleConn for the session's lifetime.
		committed := false
		var releaseWarden func()
		defer func() {
			if !committed && releaseWarden != nil {
				releaseWarden()
			}
		}()

		// Authenticate via yarilo-auth: passdb chain, brute-force penalty, token issuance.
		if s.opts.AuthAddr == "" {
			// Permanent misconfiguration, not a transient blip: close rather than
			// hold the socket open for the re-login budget.
			log.Error("login: auth_addr not configured")
			// Closing path: announce the close, not a keep-open transient code.
			writeProtoClose(authConn, s.opts.Protocol, "service temporarily unavailable")
			s.incResult("unavailable")
			return outcomeClose, nil
		}
		// Shared multiplexed client — no per-login handshake. The phase metric
		// reads ~0 except on the first login after a pod start or auth reconnect.
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
				// Observed on failure too: a timed-out dial must show in the
				// histogram.
				s.observePhase(phaseAuthDial, authDialStart)
				s.incTransientExhausted(stageAuthDial)
				s.incResult("unavailable")
				return outcomeRetry, nil
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
			// Single point where a forwarded address replaces the socket IP;
			// auth, allow_nets, warden, and the backend preamble ADDR= all
			// inherit it. Applied only when the socket peer is inside
			// general.xclient.trusted_nets. Runs at the top of the retry loop so
			// a forward arriving in a retry iteration is honoured too.
			if pre.forwardIP != "" && clientIP != pre.forwardIP {
				if ipInNets(clientIP, s.opts.XClientNets) {
					log = log.With("orig_ip", clientIP, "fwd_ip", pre.forwardIP, "fwd_port", pre.forwardPort, "fwd_via", pre.forwardSource)
					log.Info("login: client ip forwarded")
					clientIP = pre.forwardIP
				} else if pre.forwardSource == "xclient" {
					// An untrusted peer claiming to be a proxy is an anomaly.
					log.Warn("login: ignoring XCLIENT from untrusted peer", "peer_ip", clientIP, "claimed_ip", pre.forwardIP)
					pre.forwardIP = ""
				} else {
					// Bare IMAP ID with x-originating-ip is routine MUA chatter;
					// Debug to avoid log spam.
					log.Debug("login: ignoring forwarded ID from untrusted peer", "peer_ip", clientIP, "claimed_ip", pre.forwardIP)
					pre.forwardIP = ""
				}
			}

			var aerr error
			// Retry temp-fails here rather than surfacing a passdb blip. Safe to
			// repeat: internal failures do not touch the auth-penalty counter.
			for tfAttempt := 0; ; tfAttempt++ {
				authStart := time.Now()
				authResult, aerr = authCl.AuthenticateAs(pre.authzid, pre.username, pre.password, wardenService(s.opts.Protocol), clientIP, sessID)
				// One observation per attempt: each is its own round-trip.
				s.observePhase(phaseAuth, authStart)
				if !errors.Is(aerr, authclient.ErrTempFail) {
					break
				}
				if tfAttempt >= s.transientRetries() {
					log.Warn("login: auth temp fail", "user", pre.username, "attempts", tfAttempt+1, "result", "fail")
					writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
					s.incTransientExhausted(stageAuth)
					s.incResult("unavailable")
					return outcomeRetry, nil
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
				// Tagged NO [AUTHENTICATIONFAILED] then * BYE: the last LOGIN must
				// get a tagged reply before the close announcement.
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeAuthenticationFail, "Too many failed authentications")
				writeProtoClose(authConn, s.opts.Protocol, "closing")
				return outcomeClose, nil
			}
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeAuthenticationFail, "Authentication failed.")

			var retryExtTLS *tls.Config
			if _, ok := authConn.(*tls.Conn); !ok {
				retryExtTLS = s.opts.StarttlsTLS
			}
			pre, authConn, authRd, err = continueAuth(authConn, authRd, retryExtTLS, s.opts.Protocol, s.opts)
			if err != nil {
				log.Debug("login: preamble retry", "err", err)
				return outcomeClose, nil
			}
			log.Info("login: auth retry", "user", pre.username, "attempt", attempt+1)
		}

		// From here on the session belongs to the identity the auth service
		// RESOLVED, not to the string the client typed. They differ for a
		// master login in the separator form -- the client sends
		// "target*master" and the service answers user=target -- and using the
		// raw string downstream routed by it, counted connections against it,
		// and claimed it to the backend, whose VERIFY compares against the
		// identity the token was issued for and refused the session (#1306).
		//
		// Resolved once, here, so no later step can pick the wrong one. The
		// claimed string stays in the log lines that are about what the client
		// sent, and the audit lines that name both identities are unchanged.
		authUser := resolvedIdentity(authResult, pre.username)
		if authUser != pre.username {
			log.Info("login: acting as the resolved identity",
				"claimed", pre.username, "user", authUser)
		}

		// Find backend address: fixed addr (standalone) or director LOOKUP.
		// tag is hoisted so the fast-fail re-route below can re-LOOKUP with it.
		var backendAddr, tag string
		if s.opts.BackendAddr != "" {
			backendAddr = s.opts.BackendAddr
		} else {
			// Per-user director_tag from passdb/userdb wins over the static Tag
			// config, so a shared login fleet can route users to different pools.
			tag = s.opts.Tag
			if authResult.DirectorTag != "" {
				tag = authResult.DirectorTag
			}
			var err error
			lookupStart := time.Now()
			backendAddr, err = s.directorLookupWithHold(authUser, tag, log)
			s.observePhase(phaseDirectorLookup, lookupStart)
			if err != nil {
				log.Warn("login: director lookup failed", "user", pre.username, "err", err)
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
				s.incResult("unavailable")
				return outcomeRetry, nil
			}
		}

		// Warden connection limit check over the shared pool (no per-session
		// dial); the phase metric measures one round trip.
		if s.opts.WardenAddr != "" {
			wardenStart := time.Now()
			ap := s.wardenClient()
			svc := wardenService(s.opts.Protocol)
			cerr := ap.Connect(sessID, authUser, clientIP, svc)
			s.observePhase(phaseWardenConnect, wardenStart)
			switch {
			case errors.Is(cerr, warden.ErrTooManyConns):
				log.Warn("login: warden", "user", pre.username, "result", "fail", "reason", "too_many_connections")
				// The over-limit code, then the close announcement (#928
				// consistency) so IMAP/ManageSieve announce the close with a BYE
				// rather than dropping the socket after the tagged NO.
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeLimit, "too many connections")
				writeProtoClose(authConn, s.opts.Protocol, "closing")
				return outcomeClose, nil
			case cerr != nil:
				log.Error("login: warden connect failed", "addr", s.opts.WardenAddr, "err", cerr)
				if !s.opts.WardenFailOpen {
					writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
					s.incResult("unavailable")
					return outcomeRetry, nil
				}
			default:
				log.Info("login: warden", "user", pre.username, "result", "ok")
				// #814: record the routed backend in warden so `who` can scope to the
				// local backend. Best-effort.
				if beIP, _, splitErr := net.SplitHostPort(backendAddr); splitErr == nil {
					if berr := ap.Backend(sessID, beIP); berr != nil {
						log.Debug("login: warden backend push", "err", berr)
					}
				}
				hbCtx, hbCancel := context.WithCancel(context.Background())
				hbDone := make(chan struct{})
				go func() {
					defer close(hbDone)
					interval := warden.DefaultSessionTTL / 3
					if err := ap.HeartbeatLoop(hbCtx, sessID, interval, nil); err != nil {
						log.Debug("login: warden heartbeat loop", "err", err)
					}
				}()
				// The pool outlives the session, so there is nothing to close here —
				// only the registration to release. Captured as releaseWarden rather
				// than deferred directly: on a transient failure before bring-up the
				// closure's committed-defer runs it (releasing the slot before the
				// re-LOGIN), and on success it is handed to handleConn to defer for
				// the whole proxied session (#896).
				releaseWarden = func() {
					hbCancel()
					<-hbDone
					if err := ap.Disconnect(sessID, authUser, clientIP, svc); err != nil {
						log.Debug("login: warden disconnect", "err", err)
					}
				}
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
			bs, berr = s.openBackendSession(pre, authResult, authUser, tag, backendAddr, clientIP, sessID, log)
			if berr == nil {
				break
			}
			if attempt >= retries {
				log.Error("login: backend session failed", "addr", backendAddr, "attempts", attempt+1, "err", berr)
				s.observePhase(phaseBackendDial, backendDialStart)
				s.incTransientExhausted(stageBackendSession)
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
				s.incResult("unavailable")
				return outcomeRetry, nil
			}
			log.Warn("login: backend session failed, retrying", "addr", backendAddr, "attempt", attempt+1, "err", berr)
			s.incTransientRetry(stageBackendSession)
			time.Sleep(transientRetryBackoff)
		}
		s.observePhase(phaseBackendDial, backendDialStart)

		committed = true
		return outcomeDone, &established{bs: bs, user: authUser, releaseWarden: releaseWarden}
	}

	// Transient re-login loop (#896): a transient failure keeps the connection
	// open (a tagged NO [UNAVAILABLE] was sent) and returns to the pre-auth
	// command loop so the client can LOGIN again on this same connection — no new
	// TCP + TLS handshake. Bounded by transient_relogin_cap so a wedged backend
	// cannot accumulate sockets; the per-connection deadline is the other bound.
	// This budget is independent of AuthMaxAttempts (bad passwords).
	for reloginCount := 0; ; {
		outcome, e := attempt()
		if outcome == outcomeDone {
			est = e
			break
		}
		if outcome == outcomeClose {
			return
		}
		// outcomeRetry.
		reloginCount++
		if reloginCount >= s.transientReloginCap() {
			// The transient budget is spent; this connection closes now, so
			// announce it (#928) instead of dropping the socket right after the
			// keep-open NO/454 the failing attempt already sent.
			writeProtoClose(authConn, s.opts.Protocol, "too many transient failures, closing")
			return
		}
		var retryExtTLS *tls.Config
		if _, ok := authConn.(*tls.Conn); !ok {
			retryExtTLS = s.opts.StarttlsTLS
		}
		var cerr error
		pre, authConn, authRd, cerr = continueAuth(authConn, authRd, retryExtTLS, s.opts.Protocol, s.opts)
		if cerr != nil {
			log.Debug("login: transient re-login: client did not retry", "err", cerr)
			return
		}
		log.Info("login: transient re-login", "attempt", reloginCount+1)
	}

	backendConn, backendRd, backendAddr, backendCaps := est.bs.conn, est.bs.rd, est.bs.addr, est.bs.caps
	defer backendConn.Close()
	if est.releaseWarden != nil {
		defer est.releaseWarden()
	}

	// Register the session for kick support only once it is actually up — a
	// bring-up that never completed has nothing to kick.
	// Keyed by the identity the session acts as: a kick for the target must
	// find it, and a master session filed under "target*master" is invisible
	// to every administrative command (#1306).
	backendIP, _, _ := net.SplitHostPort(backendAddr)
	sess := &liveSession{
		id: sessID, user: est.user, backendConn: backendConn, clientConn: authConn,
		backendIP: backendIP, proto: s.opts.Protocol.Base(),
	}
	s.announceMu.Lock()
	s.sessMu.Lock()
	s.sessions[est.user] = append(s.sessions[est.user], sess)
	s.sessMu.Unlock()
	defer func() {
		s.sessMu.Lock()
		list := s.sessions[est.user]
		for i, v := range list {
			if v == sess {
				s.sessions[est.user] = append(list[:i], list[i+1:]...)
				break
			}
		}
		s.sessMu.Unlock()
	}()

	s.announceSessionLocked(sess)
	s.announceMu.Unlock()
	// Closed through whatever watch connection is current at the time, not
	// the one captured here: after a reconnect the captured one is dead, and
	// the close would be written into it and lost (#1393).
	defer s.announceSessionClose(sessID)

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
// backendBringupTimeout bounds the backend bring-up (preamble + greeting +
// SMTP EHLO) in openBackendSession (#927). In line with the dial timeout — a
// backend that has not greeted within it is wedged, not slow — and deliberately
// a constant, not a config knob (same class as the auth-client socket bounds,
// #926). A var only so a test can shorten it.
var backendBringupTimeout = 5 * time.Second

// resolvedIdentity is the identity a session acts as once authentication has
// succeeded: what the auth service resolved, or the login string when it named
// nobody -- which keeps a deployment on an older auth service working instead
// of claiming an empty name to the backend.
func resolvedIdentity(res *authclient.AuthResult, claimed string) string {
	if res != nil && res.Username != "" {
		return res.Username
	}
	return claimed
}

func (s *Server) openBackendSession(pre *preamble, authResult *authclient.AuthResult, authUser, tag, addr, clientIP, sessID string, log *slog.Logger) (*backendSession, error) {
	// Fast-fail re-route on a connect failure in director mode (#782): report the
	// backend unreachable and re-LOOKUP.
	// The re-route re-LOOKUPs on a failed dial, so it needs the resolved
	// identity for the same reason the first lookup did: the raw login string
	// hashes to a different pod, and a master session would land on the wrong
	// one at the first retry (#1306).
	conn, addr, err := s.dialBackendWithReroute(authUser, tag, addr, log)
	if err != nil {
		return nil, fmt.Errorf("dial backend %s: %w", addr, err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	// Bound the whole bring-up — preamble write, greeting read, and (for SMTP)
	// the EHLO exchange — with a deadline (#927). Without it a backend that
	// accepts TCP but never greets (storage hang, token-Verify wedge) holds the
	// handler in readBackendGreeting forever; the incident behind #926 saw
	// handlers stuck 7-11 min. A timed-out bring-up returns an error, which the
	// caller treats as a transient failure and (after #896) answers with a keep-
	// open NO [UNAVAILABLE] rather than hanging. Cleared once established, below.
	conn.SetDeadline(time.Now().Add(backendBringupTimeout)) //nolint:errcheck

	rd := bufio.NewReaderSize(conn, 4096)

	// The backend's PreambleListener reads this line and calls yarilo-auth VERIFY.
	pre2 := loginproto.Preamble{
		Addr:      clientIP,
		SessionID: sessID,
		// The identity the token was issued for: the backend's VERIFY compares
		// the two, and a claimed name that merely looks like the login string
		// fails a session that authenticated correctly (#1306).
		User:  authUser,
		Token: authResult.Token,
		Helo:  pre.ehloLine,
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

	// Established: clear the bring-up deadline so the proxied session is not
	// bounded by it (handleConn clears deadlines again before proxying anyway).
	conn.SetDeadline(time.Time{}) //nolint:errcheck
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
	s.reannounceSessions()
	// Adding what it does not know is only half: the list below is what
	// removes what it knows and we do not.
	s.syncSessions()

	readErr := make(chan error, 1)
	go func() {
		err := s.watchReadLoop(c, wc)
		wc.failPending()
		readErr <- err
	}()

	// Reconciliation while the connection stays up. A (re)connect covers a
	// close lost to a watch outage; this covers one lost any other way, which
	// is the point of reconciling rather than patching a known path.
	syncStop := make(chan struct{})
	defer close(syncStop)
	if every := s.sessionSyncInterval(); every > 0 {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-syncStop:
					return
				case <-t.C:
					s.syncSessions()
				}
			}
		}()
	}

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
			user, ok := kickedUser(line)
			if !ok {
				// Arity is checked rather than trimmed: a form nobody wrote on
				// purpose is a protocol error and says so. Splitting off the
				// first field and ignoring the rest is what let a ring field
				// travel into the username and kick nobody, in silence (#1363).
				slog.Warn("login: malformed USER-KICKED push, ignored", "line", line)
				break
			}
			s.kickUser(user)
		case line == "PING":
			wc.pong()
		}
		// OK and other push lines silently ignored.
	}
}

// kickedUser reads the username out of a USER-KICKED push. Exactly two forms
// are accepted: the plain kick, and the move/evacuation kick whose trailing
// field names the backend being emptied. A director of this version sends the
// plain form to logins either way; the two-field form is taken during a mixed
// rollout, where an older originator still writes its ring line here.
func kickedUser(line string) (string, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || len(fields) > 3 || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

// defaultSessionSyncInterval paces the reconciliation when the operator has
// not chosen one. It bounds how long a director may count a session nobody is
// running, which is the thing being fixed -- not how fresh the count is.
const defaultSessionSyncInterval = 30 * time.Second

// sessionSyncIDsPerLine keeps a sync line inside the director's read buffer
// (4 KiB). Session ids are ~17 bytes, so a hundred per line leaves room to
// spare; a longer line would not be truncated, it would break the connection.
const sessionSyncIDsPerLine = 100

func (s *Server) sessionSyncInterval() time.Duration {
	if s.opts.SessionSyncInterval == 0 {
		return defaultSessionSyncInterval
	}
	return s.opts.SessionSyncInterval
}

// syncSessions sends the director the complete list of sessions this pod is
// running, so it can drop what it still counts and nobody has.
//
// Announcing opens and closes alone leaves a lost event wrong forever: nothing
// ever says "this is all of it". A director that missed one SESSION-CLOSE --
// because the watch was down at that instant, or for any other reason -- keeps
// a phantom that feeds least_sessions and the kill-confirm (#1393).
//
// The snapshot and the write happen under announceMu, the same lock a session
// takes to register and announce itself. Without that, a session opened
// between the snapshot and the write would have its SESSION-OPEN arrive first
// and be erased by a list taken before it existed.
func (s *Server) syncSessions() {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()

	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc == nil {
		return
	}

	s.sessMu.RLock()
	ids := make([]string, 0, len(s.sessions))
	for _, list := range s.sessions {
		for _, sess := range list {
			ids = append(ids, sess.id)
		}
	}
	s.sessMu.RUnlock()

	wc.sessionSyncStart()
	for i := 0; i < len(ids); i += sessionSyncIDsPerLine {
		end := i + sessionSyncIDsPerLine
		if end > len(ids) {
			end = len(ids)
		}
		wc.sessionSyncChunk(ids[i:end])
	}
	wc.sessionSyncEnd()
}

// announceSession tells the director this session exists. Safe to repeat: the
// director keys sessions by id, so a re-announcement after a reconnect
// replaces the record rather than adding one.
func (s *Server) announceSession(sess *liveSession) {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()
	s.announceSessionLocked(sess)
}

func (s *Server) announceSessionLocked(sess *liveSession) {
	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc == nil {
		return
	}
	wc.sessionOpen(sess.id, sess.user, sess.backendIP, sess.proto)
}

func (s *Server) announceSessionClose(sessID string) {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()
	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc == nil {
		// The session ended while no director was reachable, so this close is
		// lost -- and until the reconciliation it stayed lost forever. Counted
		// rather than swallowed: if phantoms disappear while this stays zero,
		// the reconciliation is covering some OTHER path and we would never
		// learn which (#1393).
		metricSessionCloseDropped.Inc()
		return
	}
	wc.sessionClose(sessID)
}

// reannounceSessions re-sends SESSION-OPEN for every session still running,
// after a watch connection is established.
//
// A session is announced once, to the director holding the watch at the time.
// When that director dies the login pod reconnects -- often to a different one
// -- and the session keeps running with nobody counting it. That is the mirror
// of the phantom replica: the count is then LOW, and a kill waiting for it to
// reach zero confirms immediately on a user whose session was never touched
// (#1393).
func (s *Server) reannounceSessions() {
	s.sessMu.RLock()
	live := make([]*liveSession, 0, len(s.sessions))
	for _, list := range s.sessions {
		live = append(live, list...)
	}
	s.sessMu.RUnlock()
	if len(live) == 0 {
		return
	}
	s.announceMu.Lock()
	for _, sess := range live {
		s.announceSessionLocked(sess)
	}
	s.announceMu.Unlock()
	slog.Info("login: re-announced live sessions to the director", "count", len(live))
}

// kickUser closes all active backend connections for the given username,
// causing biProxy to terminate and those sessions to be dropped.
func (s *Server) kickUser(username string) {
	s.sessMu.RLock()
	sessions := make([]*liveSession, len(s.sessions[username]))
	copy(sessions, s.sessions[username])
	s.sessMu.RUnlock()

	// Logged with the count, including zero: a kick that matched nothing is a
	// normal event on a pod that does not hold the user, and it must not look
	// the same as a kick that never arrived (#1363).
	slog.Info("login: user kick received", "user", username,
		"proto", string(s.opts.Protocol), "sessions", len(sessions))
	for _, sess := range sessions {
		sess.close("kicked by admin or move")
	}
}

// close tears down both legs of a proxied session. The reason is logged rather
// than sent: the proxy does not know where the current response ends, so a
// notice injected mid-literal would corrupt the stream a client is parsing. A
// clean TCP close is what a kick means, and every client handles it.
func (sess *liveSession) close(reason string) {
	slog.Info("login: kicking session", "user", sess.user, "session", sess.id, "reason", reason)
	sess.backendConn.Close()
	if sess.clientConn != nil {
		sess.clientConn.Close()
	}
}

// kickSession closes both legs of the session with the given id, regardless of
// which user owns it. Returns true when a matching session was found and
// closed. Silently no-ops when nothing matches — kick events are broadcast to
// every pod and only the owner reacts.
func (s *Server) kickSession(id string) bool {
	s.sessMu.RLock()
	var target *liveSession
findLoop:
	for _, list := range s.sessions {
		for _, sess := range list {
			if sess.id == id {
				target = sess
				break findLoop
			}
		}
	}
	s.sessMu.RUnlock()
	if target == nil {
		return false
	}
	target.close("kicked by session id")
	return true
}

// kickChannel is the warden pub/sub channel this login pod
// subscribes to. Keyed per-protocol so each login binary only
// wakes up for relevant events. Event payload is the session id.
func (s *Server) kickChannel() string {
	return "kick:" + string(s.opts.Protocol)
}

// startKickSubscriber spawns the per-protocol kick subscriber. No-op when
// WardenAddr is unset (single-process dev runs). The loop runs until ctx is
// cancelled; see kickSubscribeLoop for the reconnect semantics.
func (s *Server) startKickSubscriber(ctx context.Context) {
	if s.opts.WardenAddr == "" {
		return
	}
	go s.kickSubscribeLoop(ctx, s.kickChannel())
}

// kickReconnectDelay is the backoff between kick-subscriber reconnect attempts.
const kickReconnectDelay = time.Second

// kickSubscribeLoop keeps a live subscription to the warden kick channel,
// redialling and re-subscribing whenever the connection drops (#908 PR3 — the
// mirror of #946 on the subscribe side). Without this a single warden restart or
// network blip would silently and permanently deafen this login pod to kicks,
// which is security-relevant: a kick is how a compromised or relocated session
// is evicted. The loop exits only when ctx is cancelled.
func (s *Server) kickSubscribeLoop(ctx context.Context, channel string) {
	for {
		if ctx.Err() != nil {
			return
		}
		ac, err := warden.Dial(s.opts.WardenAddr, s.opts.WardenTLS, 5*time.Second)
		if err != nil {
			slog.Warn("login: kick subscribe dial failed, retrying", "addr", s.opts.WardenAddr, "err", err)
			if !sleepCtx(ctx, kickReconnectDelay) {
				return
			}
			continue
		}
		ch, err := ac.Subscribe(ctx, channel)
		if err != nil {
			ac.Close()
			slog.Warn("login: kick subscribe failed, retrying", "channel", channel, "err", err)
			if !sleepCtx(ctx, kickReconnectDelay) {
				return
			}
			continue
		}
		slog.Info("login: kick subscriber connected", "channel", channel)
		for sessID := range ch {
			if !s.kickSession(sessID) {
				slog.Debug("login: kick event ignored (no match)", "session", sessID)
			}
		}
		ac.Close()
		// ch closed: either ctx cancel (exit) or a transport drop (reconnect).
		if ctx.Err() != nil {
			return
		}
		slog.Warn("login: kick subscription dropped, reconnecting", "channel", channel)
		if !sleepCtx(ctx, kickReconnectDelay) {
			return
		}
	}
}

// sleepCtx sleeps for d unless ctx is cancelled first; returns false if ctx was
// cancelled (caller should stop).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
		// A transient failure keeps the connection open (#896/#928), so it must
		// NOT announce a close: 421 means "closing transmission channel" (RFC
		// 5321) and a compliant client hangs up. 454 is the temporary
		// authentication failure (RFC 4954) that leaves the session open.
		switch imapCode {
		case imapCodeUnavailable:
			fmt.Fprintf(conn, "454 4.7.0 %s\r\n", msg) //nolint:errcheck
		default:
			fmt.Fprintf(conn, "421 4.3.0 %s\r\n", msg) //nolint:errcheck
		}
	case ProtocolManageSieve:
		// Likewise BYE announces a close (RFC 5804): a client that gets it hangs
		// up, defeating the #896 keep-open. Transient → NO (TRYLATER); over-limit
		// → plain NO (it precedes a close, but NO+close is the conventional reply).
		switch imapCode {
		case imapCodeAuthenticationFail:
			fmt.Fprintf(conn, "NO (AUTHENTICATIONFAILED) %q\r\n", msg) //nolint:errcheck
		case imapCodeUnavailable:
			fmt.Fprintf(conn, "NO (TRYLATER) %q\r\n", msg) //nolint:errcheck
		default:
			fmt.Fprintf(conn, "NO %q\r\n", msg) //nolint:errcheck
		}
	}
}

// writeProtoClose sends a protocol-correct close announcement — the message that
// legitimately precedes closing the connection (#928). Its counterpart
// writeProtoError sends the per-command "keep the connection open" form; keeping
// the two apart is the whole point: a transient failure must not be answered
// with a close notice while #896 holds the socket open for a retry, and a real
// close (the transient_relogin_cap, a permanent misconfiguration) must announce
// itself rather than dropping the socket unannounced.
func writeProtoClose(conn net.Conn, p Protocol, msg string) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		fmt.Fprintf(conn, "* BYE %s\r\n", msg) //nolint:errcheck
	case ProtocolPOP3, ProtocolPOP3S:
		fmt.Fprintf(conn, "-ERR [SYS/TEMP] %s\r\n", msg) //nolint:errcheck
	case ProtocolSubmission, ProtocolSubmissions:
		fmt.Fprintf(conn, "421 4.3.0 %s\r\n", msg) //nolint:errcheck
	case ProtocolManageSieve:
		fmt.Fprintf(conn, "BYE %q\r\n", msg) //nolint:errcheck
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

// wardenService maps a login Protocol to the service name used in the warden protocol.
func wardenService(p Protocol) string {
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
