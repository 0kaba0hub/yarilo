package loginproto

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	masterclient "github.com/yarilomail/yarilo/pkg/authclient"
)

// PreambleConn wraps a net.Conn after the YARILO preamble has been read
// and the session token has been verified. It exposes preamble fields plus
// the userdb-resolved storage information, and overrides RemoteAddr with
// the real client IP forwarded by the login pod.
//
// Read is proxied through the buffered reader that was used to read the
// preamble so no bytes are lost.
type PreambleConn struct {
	net.Conn
	Username  string
	SessionID string
	Service   string
	Helo      string
	// Home is the user's mail home directory from userdb.
	Home string
	// MailLoc is the mail_location override from userdb (empty = use global default).
	MailLoc string
	// Groups are the supplementary group names from userdb (used for ACL resolution).
	Groups []string
	// QuotaRules are the per-user quota rules from userdb.
	QuotaRules []string
	// QuotaOverFlag is the userdb quota_over_flag value (quota_over_status).
	QuotaOverFlag string
	// VolatileDir is the VOLATILEDIR modifier from userdb (empty = use global default).
	VolatileDir string
	// IndexDir is the INDEX= modifier from userdb (empty = co-located with mailbox).
	IndexDir string
	// ControlDir is the CONTROL= modifier from userdb (empty = co-located with mailbox).
	ControlDir string
	// AltDir is the ALT= modifier from userdb (empty = single-tier storage).
	AltDir string
	// MailPath is the base mail storage path from userdb (empty = use Home).
	MailPath string
	// InboxPath overrides INBOX location (empty = use MailPath).
	InboxPath string
	// MailboxFormat is the per-user storage driver from userdb (mail_driver /
	// mailbox_format); empty uses the driver from MailLoc, then the global one.
	MailboxFormat string
	realAddr      net.Addr
	br            *bufio.Reader
}

// UnwrapPreambleConn walks a net.Conn wrapper chain (each wrapper exposing
// Unwrap() net.Conn) to the underlying *PreambleConn, or nil if none. Servers
// use it to recover the pre-authenticated session state when listener wrappers
// (line-length/greeting/TLS) sit above the PreambleListener; a direct type
// assertion misses those.
func UnwrapPreambleConn(c net.Conn) *PreambleConn {
	type unwrapper interface{ Unwrap() net.Conn }
	for c != nil {
		if pc, ok := c.(*PreambleConn); ok {
			return pc
		}
		uw, ok := c.(unwrapper)
		if !ok {
			return nil
		}
		c = uw.Unwrap()
	}
	return nil
}

// RemoteAddr returns the real client IP forwarded in the preamble.
func (c *PreambleConn) RemoteAddr() net.Addr { return c.realAddr }

// Read reads from the buffered reader (preserving any bytes already buffered
// after the preamble line).
func (c *PreambleConn) Read(b []byte) (int, error) { return c.br.Read(b) }

// PreambleListener wraps a net.Listener. Each accepted TCP connection is
// handed to a goroutine that reads the YARILO preamble, verifies the session
// token with yarilo-auth, and optionally performs a userdb lookup via the
// master socket. Completed handshakes are delivered through a buffered channel
// so that multiple handshakes proceed in parallel and Accept never blocks on
// network I/O.
//
// When ExpectedService is non-empty, the service returned by VERIFY must match
// it exactly; mismatches prevent LMTP tokens from being replayed on IMAP and
// vice-versa.
type PreambleListener struct {
	net.Listener
	// AuthAddr is the yarilo-auth login-protocol address for VERIFY.
	AuthAddr string
	AuthTLS  *tls.Config
	// MasterAddr is the yarilo-auth master-protocol address for userdb lookup.
	MasterAddr string
	MasterTLS  *tls.Config
	// ExpectedService, when non-empty, must match the service in the VERIFY response.
	ExpectedService string
	// TLSConfig, when set, terminates internal mTLS on each accepted connection
	// BEFORE the YARILO preamble is read. The login pods wrap the backend
	// session dial in mTLS, so the backend must terminate it here; otherwise the
	// TLS ClientHello bytes are read as a preamble and every login fails.
	TLSConfig *tls.Config

	startOnce sync.Once
	ready     chan acceptResult
	ctx       context.Context
	cancel    context.CancelFunc

	// authMu guards the shared yarilo-auth client used for token VERIFY. One
	// multiplexed connection serves every session on the pod; the VERIFY wire
	// protocol carries a request id, so requests interleave safely.
	authMu sync.Mutex
	authCl *authclient.Client
}

type acceptResult struct {
	conn net.Conn
	err  error
}

// preambleChanBuf is the number of completed handshakes that can queue before
// Accept drains them. Sized to absorb a burst of concurrent logins without
// blocking the handshake goroutines.
const preambleChanBuf = 64

func (l *PreambleListener) init() {
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.ready = make(chan acceptResult, preambleChanBuf)
	go l.acceptLoop()
}

// acceptLoop runs the underlying listener's Accept in a dedicated goroutine
// and fans each raw connection out to a per-connection handshake goroutine.
func (l *PreambleListener) acceptLoop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.ready <- acceptResult{err: err}:
			case <-l.ctx.Done():
			}
			return
		}
		go l.doHandshake(c)
	}
}

func (l *PreambleListener) doHandshake(c net.Conn) {
	pc, err := l.handshake(c)
	if err != nil {
		slog.Debug("loginproto: preamble handshake failed", "remote", c.RemoteAddr(), "err", err)
		c.Close()
		return
	}
	select {
	case l.ready <- acceptResult{conn: pc}:
	case <-l.ctx.Done():
		pc.Close()
	}
}

// Accept returns the next successfully handshaked connection. It never blocks
// on network I/O — handshakes proceed in parallel goroutines.
func (l *PreambleListener) Accept() (net.Conn, error) {
	l.startOnce.Do(l.init)
	select {
	case r := <-l.ready:
		return r.conn, r.err
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

// Close cancels the context (causing any blocked Accept to return) and closes
// the underlying listener, which unblocks the internal acceptLoop.
func (l *PreambleListener) Close() error {
	l.cancel()
	l.authMu.Lock()
	cl := l.authCl
	l.authCl = nil
	l.authMu.Unlock()
	if cl != nil {
		_ = cl.Close()
	}
	return l.Listener.Close()
}

// authClient returns the shared VERIFY client, dialling it on first use. A dial
// failure leaves the field nil so the next session retries — the backend does
// not need yarilo-auth reachable at start.
func (l *PreambleListener) authClient() (*authclient.Client, error) {
	l.authMu.Lock()
	defer l.authMu.Unlock()
	if l.authCl != nil {
		return l.authCl, nil
	}
	cl, err := authclient.Dial(l.AuthAddr, l.AuthTLS)
	if err != nil {
		return nil, err
	}
	l.authCl = cl
	return cl, nil
}

const preambleReadTimeout = 5 * time.Second

func (l *PreambleListener) handshake(c net.Conn) (*PreambleConn, error) {
	c.SetDeadline(time.Now().Add(preambleReadTimeout)) //nolint:errcheck

	// Terminate internal mTLS first: the login dialled us over mTLS, so the
	// preamble arrives inside the TLS session. The read deadline above also
	// bounds the handshake.
	if l.TLSConfig != nil {
		tconn := tls.Server(c, l.TLSConfig)
		if err := tconn.Handshake(); err != nil {
			return nil, fmt.Errorf("internal mtls handshake: %w", err)
		}
		c = tconn
	}

	br := bufio.NewReader(c)
	pre, err := Parse(br)
	if err != nil {
		return nil, fmt.Errorf("preamble read: %w", err)
	}

	c.SetDeadline(time.Time{}) //nolint:errcheck

	authCl, err := l.authClient()
	if err != nil {
		return nil, fmt.Errorf("auth dial: %w", err)
	}

	username, sessionID, service, err := authCl.Verify(pre.Token, pre.User, pre.SessionID)
	if err != nil {
		return nil, fmt.Errorf("token verify: %w", err)
	}
	if username == "" {
		return nil, fmt.Errorf("token verify: empty username")
	}
	if l.ExpectedService != "" && service != l.ExpectedService {
		return nil, fmt.Errorf("token verify: service mismatch: got %q want %q", service, l.ExpectedService)
	}

	var home, mailLoc, volatileDir, indexDir, controlDir, altDir, mailPath, inboxPath, mailboxFormat string
	var quotaOverFlag string
	var groups, quotaRules []string
	if l.MasterAddr != "" {
		masterCl, merr := masterclient.Dial(l.MasterAddr, l.MasterTLS)
		if merr != nil {
			return nil, fmt.Errorf("master dial: %w", merr)
		}
		defer masterCl.Close()

		ui, merr := masterCl.Userdb(context.Background(), username)
		if merr != nil {
			return nil, fmt.Errorf("userdb lookup: %w", merr)
		}
		if ui == nil {
			return nil, fmt.Errorf("userdb lookup: user not found: %s", username)
		}
		home = ui.Home
		mailLoc = ui.MailLocation
		groups = ui.Groups
		quotaRules = ui.QuotaRules
		quotaOverFlag = ui.QuotaOverFlag
		volatileDir = ui.VolatileDir
		indexDir = ui.IndexDir
		controlDir = ui.ControlDir
		altDir = ui.AltDir
		mailPath = ui.MailPath
		inboxPath = ui.InboxPath
		mailboxFormat = ui.MailboxFormat
	}

	var realAddr net.Addr = c.RemoteAddr()
	if pre.Addr != "" {
		if ip := net.ParseIP(pre.Addr); ip != nil {
			realAddr = &net.TCPAddr{IP: ip}
		}
	}

	return &PreambleConn{
		Conn:          c,
		Username:      username,
		SessionID:     sessionID,
		Service:       service,
		Helo:          pre.Helo,
		Home:          home,
		MailLoc:       mailLoc,
		Groups:        groups,
		QuotaRules:    quotaRules,
		QuotaOverFlag: quotaOverFlag,
		VolatileDir:   volatileDir,
		IndexDir:      indexDir,
		ControlDir:    controlDir,
		AltDir:        altDir,
		MailPath:      mailPath,
		InboxPath:     inboxPath,
		MailboxFormat: mailboxFormat,
		realAddr:      realAddr,
		br:            br,
	}, nil
}
