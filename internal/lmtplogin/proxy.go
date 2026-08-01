// Package lmtplogin implements the yarilo-lmtp-login service.
//
// It accepts LMTP connections from MTAs (e.g. Postfix), performs
// per-recipient warden CONNECT and yarilo-auth SESSION token issuance, then
// at DATA time fans out one backend connection per recipient — each
// preceded by a YARILO preamble carrying the recipient's session token.
package lmtplogin

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goSmtp "github.com/emersion/go-smtp"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/cluster/proto"
	"github.com/0kaba0hub/yarilo/internal/lmtpreply"
	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/internal/warden"
	"github.com/0kaba0hub/yarilo/pkg/authclient"
)

// Options configures the lmtp-login proxy.
type Options struct {
	// Hostname is the LHLO name presented to both the MTA (on greeting) and
	// the backend (on the forwarded LHLO).
	Hostname string

	// BackendAddr is the TCP address of the LMTP backend used in standalone
	// mode. Ignored when DirectorAddr is set.
	BackendAddr string
	// BackendTimeout caps each backend dial and transaction. Default: 300s.
	BackendTimeout time.Duration
	// BackendTLS optionally wraps the backend fan-out dial with internal mTLS,
	// matching the other login proxies (#739). nil = plain TCP.
	BackendTLS *tls.Config

	// DirectorAddr is the yarilo-director address for per-recipient LOOKUP.
	// When set, each RCPT TO triggers a LOOKUP to resolve the backend pod
	// address; BackendAddr is ignored.
	DirectorAddr string
	// DirectorTLS is the mTLS config for connecting to yarilo-director.
	DirectorTLS *tls.Config
	// DirectorTag restricts LOOKUP to backends with this tag (#737).
	// "" = the untagged pool, not "any tag" — there is no full-ring mode.
	DirectorTag string
	// BackendPort overrides the port in the LOOKUP result. 0 = use as-is.
	BackendPort int
	// LocalIP is sent in the ME handshake with yarilo-director.
	LocalIP string

	// AuthMasterAddr is the yarilo-auth master-protocol address used to issue
	// SESSION tokens. Required for token issuance.
	AuthMasterAddr string
	// AuthMasterTLS optionally wraps the auth master dialer with mTLS.
	AuthMasterTLS *tls.Config

	// WardenAddr is the yarilo-warden address for per-recipient CONNECT /
	// DISCONNECT and the optional cluster-wide concurrency gate. Empty
	// disables warden integration; deliveries proceed without concurrency
	// tracking (acceptable for single-pod dev / unit tests).
	WardenAddr string
	// WardenTLS optionally wraps the warden dialer with mTLS.
	WardenTLS *tls.Config

	// ConcurrencyLimit is the maximum number of concurrent in-flight
	// deliveries to the same recipient across the cluster. -1 means
	// unlimited (CONNECT still fires, but no LOOKUP gate). Default: 10.
	ConcurrencyLimit int

	// ReadTimeout / WriteTimeout for the MTA-facing side.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// HAProxy enables PROXY protocol v1/v2 header reading from trusted
	// upstreams (#742), mirroring the other login proxies — a Postfix relay in
	// front of lmtp-login can forward the ORIGINAL SMTP client's IP this way.
	HAProxy        bool
	HAProxyTimeout time.Duration
	HAProxyNets    []*net.IPNet

	// XClient enables the inbound XCLIENT command (#742) so a trusted relay
	// forwards the original client's IP (critical for a Postfix relay in front,
	// which can only convey it via XCLIENT). A forward is honoured only when the
	// socket peer is inside XClientNets (general.xclient.trusted_nets).
	XClient     bool
	XClientNets []*net.IPNet
}

// ErrTooManyConcurrent is returned when the cluster-wide delivery count for a
// recipient is already at ConcurrencyLimit.
var ErrTooManyConcurrent = errors.New("lmtplogin: too many concurrent deliveries for user")

// Server is an LMTP login proxy.
type Server struct {
	srv  *goSmtp.Server
	opts Options
}

// New builds a Server from opts.
func New(opts Options) *Server {
	if opts.BackendTimeout == 0 {
		opts.BackendTimeout = 300 * time.Second
	}
	if opts.ConcurrencyLimit == 0 {
		opts.ConcurrencyLimit = 10
	}
	s := &Server{opts: opts}
	be := &backend{opts: opts}

	srv := goSmtp.NewServer(be)
	srv.Domain = opts.Hostname
	srv.LMTP = true
	srv.ReadTimeout = opts.ReadTimeout
	srv.WriteTimeout = opts.WriteTimeout
	srv.EnableXCLIENT = opts.XClient

	s.srv = srv
	return s
}

// Serve accepts LMTP connections on ln until it is closed.
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
	mode := s.opts.BackendAddr
	if s.opts.DirectorAddr != "" {
		mode = "director:" + s.opts.DirectorAddr
	}
	slog.Info("lmtplogin: listening", "addr", ln.Addr().String(),
		"backend", mode,
		"warden", s.opts.WardenAddr != "",
	)
	return s.srv.Serve(ln)
}

// ---- backend ----------------------------------------------------------------

type backend struct{ opts Options }

func (b *backend) NewSession(c *goSmtp.Conn) (goSmtp.Session, error) {
	peerIP := ""
	if c != nil {
		if raw := c.Conn(); raw != nil {
			if h, _, err := net.SplitHostPort(raw.RemoteAddr().String()); err == nil {
				peerIP = h
			} else {
				peerIP = raw.RemoteAddr().String()
			}
		}
	}
	return &session{opts: b.opts, peerIP: peerIP, socketIP: peerIP}, nil
}

// ---- session ----------------------------------------------------------------

type rcptEntry struct {
	to          string // original RCPT TO value
	username    string // canonical user@domain (no plus-detail)
	wardenID    string // warden session handle (empty if warden skipped)
	token       string // one-time session token from yarilo-auth
	backendAddr string // resolved backend address (per-recipient in director mode)
}

type session struct {
	opts Options
	// socketIP is the immutable TCP peer (proxyproto-rewritten if HAProxy ran);
	// it is what the XCLIENT trust check is made against. peerIP starts equal
	// and is overridden by a trusted inbound XCLIENT (#742).
	socketIP string
	peerIP   string
	from     string
	rcpts    []rcptEntry

	// wardenConn is dialled lazily on the first RCPT TO that needs it.
	// One connection reused for all RCPTs in this MTA session.
	wardenMu   sync.Mutex
	wardenConn *warden.Conn
	wardenErr  error // sticky dial failure

	// authCl is the yarilo-auth master client, dialled lazily.
	authMu  sync.Mutex
	authCl  *authclient.Client
	authErr error // sticky dial failure

	// reqID generates the LOOKUP correlation id (#741 — previously sent
	// empty, unlike internal/login's per-request counter).
	reqID atomic.Uint64
}

func (s *session) Mail(from string, _ *goSmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	username := rcptUsername(to)
	if username == "" {
		return &goSmtp.SMTPError{Code: 501, EnhancedCode: goSmtp.EnhancedCode{5, 1, 3}, Message: "Bad recipient address"}
	}

	// Resolve backend address before reserving any resources.
	backendAddr, err := s.resolveBackend(username)
	if err != nil {
		slog.Error("lmtplogin: backend lookup failed", "user", username, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 4, 0}, Message: "Backend routing error"}
	}

	// Warden CONNECT: register delivery and optionally gate concurrency.
	wardenID, err := s.wardenConnect(username)
	if errors.Is(err, ErrTooManyConcurrent) {
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Too many concurrent deliveries for user"}
	}
	if err != nil {
		slog.Warn("lmtplogin: warden unavailable, accepting without cluster limit", "user", username, "err", err)
	}

	// Issue a session token for this recipient.
	tok, err := s.issueToken(username, wardenID)
	if err != nil {
		if wardenID != "" {
			s.wardenDisconnect(wardenID, username)
		}
		slog.Error("lmtplogin: session token issue failed", "user", username, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary auth error"}
	}

	s.rcpts = append(s.rcpts, rcptEntry{to: to, username: username, wardenID: wardenID, token: tok, backendAddr: backendAddr})
	return nil
}

// Data is never called in LMTP mode.
func (s *session) Data(_ io.Reader) error { return nil }

// LMTPData fans out one backend connection per recipient and collects
// per-recipient status for the MTA.
func (s *session) LMTPData(r io.Reader, status goSmtp.StatusCollector) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	type result struct {
		to  string
		err error
	}
	results := make(chan result, len(s.rcpts))
	var wg sync.WaitGroup
	for _, e := range s.rcpts {
		wg.Add(1)
		go func(e rcptEntry) {
			defer wg.Done()
			pre := loginproto.Preamble{
				Addr:      s.peerIP,
				SessionID: e.wardenID,
				User:      e.username,
				Token:     e.token,
			}
			rerr := fanOutOne(e.backendAddr, s.opts.Hostname, s.from, e.to, data, pre, s.opts.BackendTimeout, s.opts.BackendTLS)
			if rerr == nil {
				slog.Info("lmtplogin: delivered", "rcpt", e.to, "size", len(data))
			} else {
				slog.Error("lmtplogin: delivery failed", "rcpt", e.to, "err", rerr)
			}
			results <- result{to: e.to, err: rerr}
		}(e)
	}
	wg.Wait()
	close(results)

	for res := range results {
		var smtpErr error
		if res.err != nil {
			if smtp, ok := res.err.(*goSmtp.SMTPError); ok {
				smtpErr = smtp
			} else {
				smtpErr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: "Delivery failed"}
			}
		}
		status.SetStatus(res.to, smtpErr)
	}

	// Release warden slots for all recipients regardless of outcome.
	for _, e := range s.rcpts {
		if e.wardenID != "" {
			s.wardenDisconnect(e.wardenID, e.username)
		}
	}
	s.rcpts = nil
	return nil
}

func (s *session) Reset() {
	for _, e := range s.rcpts {
		if e.wardenID != "" {
			s.wardenDisconnect(e.wardenID, e.username)
		}
	}
	s.rcpts = nil
	s.from = ""
}

func (s *session) Logout() error {
	s.Reset()
	s.wardenMu.Lock()
	if s.wardenConn != nil {
		s.wardenConn.Close()
		s.wardenConn = nil
	}
	s.wardenMu.Unlock()
	s.authMu.Lock()
	if s.authCl != nil {
		s.authCl.Close()
		s.authCl = nil
	}
	s.authMu.Unlock()
	return nil
}

// ---- warden helpers ----------------------------------------------------------

func (s *session) wardenConnect(user string) (string, error) {
	if s.opts.WardenAddr == "" {
		return "", nil
	}
	s.wardenMu.Lock()
	defer s.wardenMu.Unlock()
	if s.wardenConn == nil {
		if s.wardenErr != nil {
			return "", s.wardenErr
		}
		c, err := warden.Dial(s.opts.WardenAddr, s.opts.WardenTLS, 5*time.Second)
		if err != nil {
			s.wardenErr = fmt.Errorf("lmtplogin/warden: dial: %w", err)
			return "", s.wardenErr
		}
		s.wardenConn = c
	}
	limit := s.opts.ConcurrencyLimit
	if limit > 0 {
		count, err := s.wardenConn.Lookup(user, "lmtp")
		if err != nil {
			return "", fmt.Errorf("lmtplogin/warden: lookup: %w", err)
		}
		if count >= limit {
			return "", ErrTooManyConcurrent
		}
	}
	id := newSessionID()
	if err := s.wardenConn.Connect(id, user, s.peerIP, "lmtp"); err != nil {
		return "", fmt.Errorf("lmtplogin/warden: connect: %w", err)
	}
	return id, nil
}

func (s *session) wardenDisconnect(id, user string) {
	s.wardenMu.Lock()
	defer s.wardenMu.Unlock()
	if s.wardenConn == nil {
		return
	}
	if err := s.wardenConn.Disconnect(id, user, s.peerIP, "lmtp"); err != nil {
		slog.Debug("lmtplogin/warden: disconnect", "user", user, "err", err)
	}
}

// ---- auth helpers -----------------------------------------------------------

// ensureAuthClient returns the lazily-dialled, session-lifetime yarilo-auth
// master client, dialling it on first use. Caller must hold s.authMu.
func (s *session) ensureAuthClient() (*authclient.Client, error) {
	if s.authCl != nil {
		return s.authCl, nil
	}
	if s.authErr != nil {
		return nil, s.authErr
	}
	c, err := authclient.DialContext(context.Background(), s.opts.AuthMasterAddr, s.opts.AuthMasterTLS)
	if err != nil {
		s.authErr = fmt.Errorf("lmtplogin/auth: dial master: %w", err)
		return nil, s.authErr
	}
	s.authCl = c
	return c, nil
}

func (s *session) issueToken(username, wardenID string) (string, error) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	c, err := s.ensureAuthClient()
	if err != nil {
		return "", err
	}
	tok, err := c.IssueSession(context.Background(), username, wardenID, s.peerIP)
	if err != nil {
		return "", fmt.Errorf("lmtplogin/auth: IssueSession: %w", err)
	}
	return tok, nil
}

// resolveDirectorTag looks up the per-recipient director_tag userdb field
// (#746) so a shared login fleet can route different users to different
// tag-pools. Falls back to "" (caller then uses the static opts.DirectorTag)
// on any lookup failure or when the user carries no override — a tag-lookup
// miss must never block delivery.
func (s *session) resolveDirectorTag(username string) string {
	if s.opts.AuthMasterAddr == "" {
		return ""
	}
	s.authMu.Lock()
	c, err := s.ensureAuthClient()
	s.authMu.Unlock()
	if err != nil {
		slog.Debug("lmtplogin: director_tag lookup: auth dial failed", "user", username, "err", err)
		return ""
	}
	ui, err := c.Userdb(context.Background(), username)
	if err != nil {
		slog.Debug("lmtplogin: director_tag lookup failed", "user", username, "err", err)
		return ""
	}
	if ui == nil {
		return ""
	}
	return ui.DirectorTag
}

// ---- director / backend resolution ------------------------------------------

// resolveBackend returns the backend address for the given username.
// BackendAddr (standalone) wins when both BackendAddr and DirectorAddr are
// set (#741 — unified with internal/login's existing precedence, which
// lmtplogin previously inverted). In director mode (DirectorAddr set,
// BackendAddr empty) it performs a per-recipient LOOKUP, restricted to the
// user's per-user director_tag (#746) when the userdb sets one, falling
// back to the component's static DirectorTag otherwise.
func (s *session) resolveBackend(username string) (string, error) {
	if s.opts.BackendAddr != "" {
		return s.opts.BackendAddr, nil
	}
	if s.opts.DirectorAddr == "" {
		return "", nil
	}
	tag := s.opts.DirectorTag
	if userTag := s.resolveDirectorTag(username); userTag != "" {
		tag = userTag
	}
	return s.directorLookup(username, tag)
}

// directorLookup dials yarilo-director, sends a LOOKUP for username, and
// returns the resolved backend address. BackendPort overrides the port in
// the LOOKUP result when set.
func (s *session) directorLookup(username, tag string) (string, error) {
	var dc *proto.Conn
	var err error
	if s.opts.DirectorTLS != nil {
		dc, err = proto.DialTLS(s.opts.DirectorAddr, s.opts.LocalIP, 0, s.opts.DirectorTLS)
	} else {
		dc, err = proto.Dial(s.opts.DirectorAddr, s.opts.LocalIP, 0)
	}
	if err != nil {
		return "", fmt.Errorf("lmtplogin/director: dial: %w", err)
	}
	defer dc.Close()

	id := fmt.Sprintf("%d", s.reqID.Add(1))
	res, err := dc.Lookup(id, username, tag, "lmtp")
	if err != nil {
		return "", fmt.Errorf("lmtplogin/director: lookup %s: %w", username, err)
	}

	addr := res.Addr
	if s.opts.BackendPort != 0 {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		addr = fmt.Sprintf("%s:%d", host, s.opts.BackendPort)
	}
	return addr, nil
}

// ---- backend fan-out --------------------------------------------------------

// dialBackend opens the backend fan-out connection, wrapping it in internal
// mTLS when tlsCfg is set (#739 — parity with the other login proxies).
func dialBackend(addr string, timeout time.Duration, tlsCfg *tls.Config) (net.Conn, error) {
	if tlsCfg != nil {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("lmtplogin: mtls dial backend %s: %w", addr, err)
		}
		return conn, nil
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("lmtplogin: dial backend %s: %w", addr, err)
	}
	return conn, nil
}

// fanOutOne opens a single backend connection for rcpt, writes the preamble,
// then completes a minimal LMTP transaction: LHLO → MAIL FROM → RCPT TO → DATA.
func fanOutOne(backendAddr, hostname, from, rcpt string, data []byte, pre loginproto.Preamble, timeout time.Duration, tlsCfg *tls.Config) error {
	conn, err := dialBackend(backendAddr, timeout, tlsCfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck

	// Preamble must be written before go-smtp reads the 220 greeting from the
	// backend — the backend's PreambleListener consumes it first.
	if _, err := fmt.Fprint(conn, pre.Format()); err != nil {
		return fmt.Errorf("lmtplogin: write preamble: %w", err)
	}

	c := goSmtp.NewClientLMTP(conn)
	if err := c.Hello(hostname); err != nil {
		return fmt.Errorf("lmtplogin: LHLO: %w", err)
	}
	if err := c.Mail(from, nil); err != nil {
		return fmt.Errorf("lmtplogin: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(rcpt, nil); err != nil {
		return err
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("lmtplogin: DATA command: %w", err)
	}
	if _, err := wc.Write(data); err != nil {
		wc.Close() //nolint:errcheck
		return fmt.Errorf("lmtplogin: write body: %w", err)
	}
	perRcpt, closeErr := wc.CloseWithLMTPResponse()
	if lmtpErr, ok := closeErr.(goSmtp.LMTPDataError); ok {
		if smtpErr, found := lmtpErr[rcpt]; found {
			// Strip the backend's per-recipient "<rcpt> " prefix; this login
			// server's handleDataLMTP prepends its own, else it doubles up.
			return lmtpreply.StripRcptPrefix(smtpErr, rcpt)
		}
		return nil
	}
	if closeErr != nil {
		return fmt.Errorf("lmtplogin: data response: %w", closeErr)
	}
	_ = perRcpt
	return nil
}

// ---- helpers ----------------------------------------------------------------

// rcptUsername strips angle brackets, trims spaces, removes plus-detail
// extension (user+folder@domain → user@domain), and returns the canonical
// user@domain string that yarilo-auth and the mailbox backend expect.
func rcptUsername(to string) string {
	to = strings.Trim(strings.TrimSpace(to), "<>")
	at := strings.LastIndex(to, "@")
	if at < 0 {
		return ""
	}
	local, domain := to[:at], to[at+1:]
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	return local + "@" + domain
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// XClient receives the parameters of an inbound XCLIENT command from the go-smtp
// server (#742). The forwarded ADDR is honoured only when the immutable socket
// peer is inside XClientNets — the relay itself must be trusted. On success the
// forwarded IP replaces peerIP, flowing into the backend preamble ADDR=, the
// warden per-recipient CONNECT, and the issued session token.
func (s *session) XClient(a goSmtp.XClientAttrs) {
	if a.Addr == "" {
		return
	}
	if !ipInNets(s.socketIP, s.opts.XClientNets) {
		// A relay that is not in xclient.trusted_nets sending XCLIENT is an
		// anomaly — someone is claiming to be a trusted front-end.
		slog.Warn("lmtplogin: ignoring XCLIENT from untrusted peer", "peer_ip", s.socketIP, "claimed_ip", a.Addr)
		return
	}
	slog.Info("lmtplogin: client ip forwarded", "orig_ip", s.socketIP, "fwd_ip", a.Addr, "fwd_via", "xclient")
	s.peerIP = a.Addr
}

// haProxyPolicy trusts the PROXY header only from peers inside nets. Empty nets
// = ignore every PROXY header. Mirrors internal/login.
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

// ipInNets reports whether the string IP falls inside one of the CIDRs. Empty
// nets = trust nobody. Gates inbound XCLIENT on xclient.trusted_nets (#742).
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
