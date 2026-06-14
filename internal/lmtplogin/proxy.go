// Package lmtplogin implements the yarilo-lmtp-login service.
//
// It accepts LMTP connections from MTAs (e.g. Postfix), performs
// per-recipient anvil CONNECT and yarilo-auth SESSION token issuance, then
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
	"time"

	goSmtp "github.com/0kaba0hub/go-smtp"

	"github.com/0kaba0hub/yarilo/internal/anvil"
	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/pkg/authclient"
)

// Options configures the lmtp-login proxy.
type Options struct {
	// Hostname is the LHLO name presented to both the MTA (on greeting) and
	// the backend (on the forwarded LHLO).
	Hostname string

	// BackendAddr is the TCP address of the LMTP backend.
	BackendAddr string
	// BackendTimeout caps each backend dial and transaction. Default: 300s.
	BackendTimeout time.Duration

	// AuthMasterAddr is the yarilo-auth master-protocol address used to issue
	// SESSION tokens. Required for token issuance.
	AuthMasterAddr string
	// AuthMasterTLS optionally wraps the auth master dialer with mTLS.
	AuthMasterTLS *tls.Config

	// AnvilAddr is the yarilo-anvil address for per-recipient CONNECT /
	// DISCONNECT and the optional cluster-wide concurrency gate. Empty
	// disables anvil integration; deliveries proceed without concurrency
	// tracking (acceptable for single-pod dev / unit tests).
	AnvilAddr string
	// AnvilTLS optionally wraps the anvil dialer with mTLS.
	AnvilTLS *tls.Config

	// ConcurrencyLimit is the maximum number of concurrent in-flight
	// deliveries to the same recipient across the cluster. -1 means
	// unlimited (CONNECT still fires, but no LOOKUP gate). Default: 10.
	ConcurrencyLimit int

	// ReadTimeout / WriteTimeout for the MTA-facing side.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
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

	s.srv = srv
	return s
}

// Serve accepts LMTP connections on ln until it is closed.
func (s *Server) Serve(ln net.Listener) error {
	slog.Info("lmtplogin: listening", "addr", ln.Addr().String(),
		"backend", s.opts.BackendAddr,
		"anvil", s.opts.AnvilAddr != "",
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
	return &session{opts: b.opts, peerIP: peerIP}, nil
}

// ---- session ----------------------------------------------------------------

type rcptEntry struct {
	to       string // original RCPT TO value
	username string // canonical user@domain (no plus-detail)
	anvilID  string // anvil session handle (empty if anvil skipped)
	token    string // one-time session token from yarilo-auth
}

type session struct {
	opts   Options
	peerIP string
	from   string
	rcpts  []rcptEntry

	// anvilConn is dialled lazily on the first RCPT TO that needs it.
	// One connection reused for all RCPTs in this MTA session.
	anvilMu   sync.Mutex
	anvilConn *anvil.Conn
	anvilErr  error // sticky dial failure

	// authCl is the yarilo-auth master client, dialled lazily.
	authMu  sync.Mutex
	authCl  *authclient.Client
	authErr error // sticky dial failure
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

	// Anvil CONNECT: register delivery and optionally gate concurrency.
	anvilID, err := s.anvilConnect(username)
	if errors.Is(err, ErrTooManyConcurrent) {
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Too many concurrent deliveries for user"}
	}
	if err != nil {
		slog.Warn("lmtplogin: anvil unavailable, accepting without cluster limit", "user", username, "err", err)
		// Non-fatal — deliver without concurrency tracking.
	}

	// Issue a session token for this recipient.
	tok, err := s.issueToken(username, anvilID)
	if err != nil {
		if anvilID != "" {
			s.anvilDisconnect(anvilID, username)
		}
		slog.Error("lmtplogin: session token issue failed", "user", username, "err", err)
		return &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 3, 0}, Message: "Temporary auth error"}
	}

	s.rcpts = append(s.rcpts, rcptEntry{to: to, username: username, anvilID: anvilID, token: tok})
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
				SessionID: e.anvilID,
				User:      e.username,
				Token:     e.token,
			}
			rerr := fanOutOne(s.opts.BackendAddr, s.opts.Hostname, s.from, e.to, data, pre, s.opts.BackendTimeout)
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
			smtpErr = &goSmtp.SMTPError{Code: 451, EnhancedCode: goSmtp.EnhancedCode{4, 2, 0}, Message: "Delivery failed"}
		}
		status.SetStatus(res.to, smtpErr)
	}

	// Release anvil slots for all recipients regardless of outcome.
	for _, e := range s.rcpts {
		if e.anvilID != "" {
			s.anvilDisconnect(e.anvilID, e.username)
		}
	}
	s.rcpts = nil
	return nil
}

func (s *session) Reset() {
	for _, e := range s.rcpts {
		if e.anvilID != "" {
			s.anvilDisconnect(e.anvilID, e.username)
		}
	}
	s.rcpts = nil
	s.from = ""
}

func (s *session) Logout() error {
	s.Reset()
	s.anvilMu.Lock()
	if s.anvilConn != nil {
		s.anvilConn.Close()
		s.anvilConn = nil
	}
	s.anvilMu.Unlock()
	s.authMu.Lock()
	if s.authCl != nil {
		s.authCl.Close()
		s.authCl = nil
	}
	s.authMu.Unlock()
	return nil
}

// ---- anvil helpers ----------------------------------------------------------

func (s *session) anvilConnect(user string) (string, error) {
	if s.opts.AnvilAddr == "" {
		return "", nil
	}
	s.anvilMu.Lock()
	defer s.anvilMu.Unlock()
	if s.anvilConn == nil {
		if s.anvilErr != nil {
			return "", s.anvilErr
		}
		c, err := anvil.Dial(s.opts.AnvilAddr, s.opts.AnvilTLS, 5*time.Second)
		if err != nil {
			s.anvilErr = fmt.Errorf("lmtplogin/anvil: dial: %w", err)
			return "", s.anvilErr
		}
		s.anvilConn = c
	}
	limit := s.opts.ConcurrencyLimit
	if limit > 0 {
		count, err := s.anvilConn.Lookup(user, "lmtp")
		if err != nil {
			return "", fmt.Errorf("lmtplogin/anvil: lookup: %w", err)
		}
		if count >= limit {
			return "", ErrTooManyConcurrent
		}
	}
	id := newSessionID()
	if err := s.anvilConn.Connect(id, user, s.peerIP, "lmtp"); err != nil {
		return "", fmt.Errorf("lmtplogin/anvil: connect: %w", err)
	}
	return id, nil
}

func (s *session) anvilDisconnect(id, user string) {
	s.anvilMu.Lock()
	defer s.anvilMu.Unlock()
	if s.anvilConn == nil {
		return
	}
	if err := s.anvilConn.Disconnect(id, user, s.peerIP, "lmtp"); err != nil {
		slog.Debug("lmtplogin/anvil: disconnect", "user", user, "err", err)
	}
}

// ---- auth helpers -----------------------------------------------------------

func (s *session) issueToken(username, anvilID string) (string, error) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authCl == nil {
		if s.authErr != nil {
			return "", s.authErr
		}
		c, err := authclient.DialContext(context.Background(), s.opts.AuthMasterAddr, s.opts.AuthMasterTLS)
		if err != nil {
			s.authErr = fmt.Errorf("lmtplogin/auth: dial master: %w", err)
			return "", s.authErr
		}
		s.authCl = c
	}
	tok, err := s.authCl.IssueSession(context.Background(), username, anvilID, s.peerIP)
	if err != nil {
		return "", fmt.Errorf("lmtplogin/auth: IssueSession: %w", err)
	}
	return tok, nil
}

// ---- backend fan-out --------------------------------------------------------

// fanOutOne opens a single backend connection for rcpt, writes the preamble,
// then completes a minimal LMTP transaction: LHLO → MAIL FROM → RCPT TO → DATA.
func fanOutOne(backendAddr, hostname, from, rcpt string, data []byte, pre loginproto.Preamble, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", backendAddr, timeout)
	if err != nil {
		return fmt.Errorf("lmtplogin: dial backend %s: %w", backendAddr, err)
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
			return smtpErr
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
	to = strings.TrimSpace(strings.Trim(to, "<>"))
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
