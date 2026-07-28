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
type watchConn struct {
	mu sync.Mutex
	c  *proto.Conn
}

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startKickSubscriber(ctx)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("login: accept: %w", err)
		}
		go s.handleConn(conn)
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
		tlsConn := tls.Server(conn, s.opts.ExtTLS)
		if err := tlsConn.Handshake(); err != nil {
			log.Debug("login: tls handshake", "err", err)
			return
		}
		conn = tlsConn
	}

	rd := bufio.NewReaderSize(conn, 4096)

	// Extract preamble: speak the protocol pre-auth exchange to collect credentials.
	// authConn/authRd may be TLS-upgraded from the original conn/rd if STARTTLS happened.
	pre, authConn, authRd, err := extractPreamble(conn, rd, s.opts.Protocol, s.opts.StarttlsTLS, s.opts)
	if err != nil {
		log.Debug("login: preamble", "err", err)
		return
	}

	// Authenticate via yarilo-auth: passdb chain, brute-force penalty, token issuance.
	if s.opts.AuthAddr == "" {
		log.Error("login: auth_addr not configured")
		writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
		return
	}
	authCl, err := authclient.Dial(s.opts.AuthAddr, s.opts.AuthTLS)
	if err != nil {
		log.Error("login: yarilo-auth dial", "addr", s.opts.AuthAddr, "err", err)
		writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
		return
	}
	defer authCl.Close()

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
		authResult, aerr = authCl.Authenticate(pre.username, pre.password, anvilService(s.opts.Protocol), clientIP, sessID)
		if errors.Is(aerr, authclient.ErrTempFail) {
			log.Warn("login: auth temp fail", "user", pre.username, "result", "fail")
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
			return
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
		backendAddr, err = s.directorLookupWithHold(pre.username, tag, log)
		if err != nil {
			log.Warn("login: director lookup failed", "user", pre.username, "err", err)
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
			return
		}
	}

	// Anvil connection limit check.
	if s.opts.AnvilAddr != "" {
		ac, aerr := anvil.Dial(s.opts.AnvilAddr, s.opts.AnvilTLS, 0)
		if aerr != nil {
			log.Error("login: anvil dial failed", "addr", s.opts.AnvilAddr, "err", aerr)
			if !s.opts.AnvilFailOpen {
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
				return
			}
		} else {
			svc := anvilService(s.opts.Protocol)
			cerr := ac.Connect(sessID, pre.username, clientIP, svc)
			if cerr == anvil.ErrTooManyConns {
				ac.Close()
				log.Warn("login: anvil", "user", pre.username, "result", "fail", "reason", "too_many_connections")
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeLimit, "too many connections")
				return
			}
			if cerr != nil {
				ac.Close()
				log.Error("login: anvil connect failed", "err", cerr)
				if !s.opts.AnvilFailOpen {
					writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
					return
				}
			} else {
				log.Info("login: anvil", "user", pre.username, "result", "ok")
				// #814: record the routed backend in anvil so `who` can scope
				// to the local backend. Best-effort, and BEFORE the heartbeat
				// goroutine starts so ac is never written concurrently.
				if beIP, _, splitErr := net.SplitHostPort(backendAddr); splitErr == nil {
					if berr := ac.Backend(sessID, beIP); berr != nil {
						log.Debug("login: anvil backend push", "err", berr)
					}
				}
				hbCtx, hbCancel := context.WithCancel(context.Background())
				hbDone := make(chan struct{})
				go func() {
					defer close(hbDone)
					interval := anvil.DefaultSessionTTL / 3
					if err := ac.HeartbeatLoop(hbCtx, sessID, interval, nil); err != nil {
						log.Debug("login: anvil heartbeat loop", "err", err)
					}
				}()
				defer func() {
					hbCancel()
					<-hbDone
					if err := ac.Disconnect(sessID, pre.username, clientIP, svc); err != nil {
						log.Debug("login: anvil disconnect", "err", err)
					}
					ac.Close()
				}()
			}
		}
	}

	// Dial backend pod, with an active fast-fail re-route on a connect failure
	// in director mode (#782): report the backend unreachable and re-LOOKUP.
	backendConn, backendAddr, err := s.dialBackendWithReroute(pre.username, tag, backendAddr, log)
	if err != nil {
		log.Error("login: dial backend", "addr", backendAddr, "err", err)
		writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
		return
	}
	defer backendConn.Close()

	// Register session for kick support.
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

	backendRd := bufio.NewReaderSize(backendConn, 4096)

	// Send preamble to backend before its protocol greeting.
	// The backend's PreambleListener reads this line and calls yarilo-auth VERIFY.
	pre2 := loginproto.Preamble{
		Addr:      clientIP,
		SessionID: sessID,
		User:      pre.username,
		Token:     authResult.Token,
		Helo:      pre.ehloLine,
	}
	if _, err := io.WriteString(backendConn, pre2.Format()); err != nil {
		log.Error("login: send preamble", "err", err)
		writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
		return
	}

	// Read backend greeting; for IMAP extract post-auth capabilities to
	// include in the tagged OK response sent to the client.
	// If the backend closes the connection (e.g. token VERIFY failed) we must
	// tell the client rather than silently dropping the TCP connection.
	backendCaps, err := readBackendGreeting(backendRd, s.opts.Protocol)
	if err != nil {
		log.Error("login: backend rejected session", "user", pre.username, "err", err)
		writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
		return
	}

	// For SMTP submission: send EHLO so the backend's state machine has a HELO
	// domain before the client (in biProxy) sends MAIL FROM.
	if isSubmission(s.opts.Protocol) {
		ehlo := pre.ehloLine
		if ehlo == "" {
			ehlo = "EHLO yarilo-submission-login\r\n"
		}
		if _, err := io.WriteString(backendConn, ehlo); err != nil {
			log.Error("login: smtp ehlo send", "err", err)
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
			return
		}
		for {
			line, err := backendRd.ReadString('\n')
			if err != nil {
				log.Error("login: smtp ehlo resp", "err", err)
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
				return
			}
			if len(line) >= 4 && line[3] != '-' {
				break
			}
		}
	}

	log.Info("login: session routed", "user", pre.username, "backend", backendAddr, "result", "ok")

	// Auth is confirmed — tell the client before entering proxy mode.
	writeProtoAuthOK(authConn, s.opts.Protocol, pre.cmdTag, backendCaps)

	authConn.SetDeadline(time.Time{})    //nolint:errcheck
	backendConn.SetDeadline(time.Time{}) //nolint:errcheck

	biProxy(authRd, authConn, backendRd, backendConn)
	log.Info("login: disconnect", "user", pre.username)
}

// directorLookup dials yarilo-director, issues a LOOKUP restricted to tag,
// and returns the backend address.
func (s *Server) directorLookup(username, tag string) (string, error) {
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

	id := fmt.Sprintf("%d", s.reqID.Add(1))
	result, err := c.Lookup(id, username, tag, s.opts.Protocol.Base())
	if err != nil {
		return "", fmt.Errorf("director lookup: %w", err)
	}

	// Override the backend port from config (director returns ring port; we may need backend-specific port).
	if s.opts.BackendPort > 0 {
		host, _, err2 := net.SplitHostPort(result.Addr)
		if err2 == nil {
			return net.JoinHostPort(host, fmt.Sprintf("%d", s.opts.BackendPort)), nil
		}
	}
	return result.Addr, nil
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
	go func() { readErr <- s.watchReadLoop(c, wc) }()

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
