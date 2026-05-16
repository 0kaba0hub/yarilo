// Package login implements the yarilo mail-protocol login proxy.
// Each login pod accepts mail-client connections (IMAP, POP3, or SMTP Submission),
// extracts the protocol preamble to learn the authenticated username, queries the
// yarilo-director LOOKUP to find the correct backend pod, and proxies the session.
// TLS is terminated here; backends receive plain TCP (or mTLS for internal links).
package login

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/cluster/proto"
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
)

// Options configures the login proxy Server.
type Options struct {
	// Protocol is one of the Protocol constants above.
	Protocol Protocol
	// Tag restricts director LOOKUP to backends with this tag. "" = full ring.
	Tag string
	// DirectorAddr is the host:port of yarilo-director (e.g. "yarilo-director:9102").
	DirectorAddr string
	// DirectorTLS is the mTLS config for connecting to yarilo-director.
	// Nil means plain TCP.
	DirectorTLS *tls.Config
	// LocalIP is the pod IP used in the ME handshake with the director.
	LocalIP string
	// BackendPort is the containerPort on backend pods.
	BackendPort int
	// BackendTLS is the mTLS config for connecting to backend pods.
	// Nil means plain TCP.
	BackendTLS *tls.Config
	// ExtTLS is the client-facing TLS config for implicit-TLS listeners
	// (IMAPS :993, POP3S :995, Submissions :465).
	// Nil means plain-text / STARTTLS (login-pod handles STARTTLS upgrade).
	ExtTLS *tls.Config
	// HAProxy enables PROXY protocol v1/v2 header reading from trusted upstreams.
	HAProxy        bool
	HAProxyTimeout time.Duration
	HAProxyNets    []*net.IPNet
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

func (w *watchConn) sessionOpen(sessID, username, backendIP string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine(fmt.Sprintf("SESSION-OPEN\t%s\t%s\t%s", sessID, proto.TabEscape(username), backendIP))
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

// Server is the login proxy server.
type Server struct {
	opts   Options
	reqID  atomic.Uint64
	sessID atomic.Uint64

	sessMu   sync.RWMutex
	sessions map[string][]*liveSession // username → active sessions

	watchMu sync.RWMutex
	watch   *watchConn // persistent director connection for push notifications
}

// New creates a Server.
func New(opts Options) *Server {
	return &Server{
		opts:     opts,
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

	remote := conn.RemoteAddr().String()
	clientIP, _, err := net.SplitHostPort(remote)
	if err != nil {
		clientIP = remote
	}

	// Implicit-TLS upgrade (IMAPS / POP3S / Submissions).
	if s.opts.ExtTLS != nil {
		tlsConn := tls.Server(conn, s.opts.ExtTLS)
		if err := tlsConn.Handshake(); err != nil {
			slog.Debug("login: tls handshake", "proto", s.opts.Protocol, "remote", remote, "err", err)
			return
		}
		conn = tlsConn
	}

	rd := bufio.NewReaderSize(conn, 4096)

	// Extract preamble: handle pre-auth protocol exchange with mail client.
	// Returns username (for director LOOKUP) and authLines (to replay to backend).
	pre, err := extractPreamble(conn, rd, s.opts.Protocol, s.opts.ExtTLS)
	if err != nil {
		slog.Debug("login: preamble", "proto", s.opts.Protocol, "remote", remote, "err", err)
		return
	}

	// Director LOOKUP: find the backend pod for this user.
	backendAddr, err := s.directorLookup(pre.username)
	if err != nil {
		slog.Warn("login: director lookup failed", "proto", s.opts.Protocol, "user", pre.username, "err", err)
		writeProtoError(conn, s.opts.Protocol, pre.cmdTag, "backend unavailable")
		return
	}

	slog.Info("login: session routed",
		"proto", s.opts.Protocol,
		"user", pre.username,
		"remote", remote,
		"backend", backendAddr,
	)

	// Dial backend pod.
	backendConn, err := dialBackend(backendAddr, s.opts.BackendTLS)
	if err != nil {
		slog.Error("login: dial backend", "addr", backendAddr, "err", err)
		writeProtoError(conn, s.opts.Protocol, pre.cmdTag, "backend unavailable")
		return
	}
	defer backendConn.Close()

	// Register session for kick support.
	sessID := fmt.Sprintf("%d", s.sessID.Add(1))
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
		wc.sessionOpen(sessID, pre.username, backendIP)
		defer wc.sessionClose(sessID)
	}

	backendRd := bufio.NewReaderSize(backendConn, 4096)

	// Discard backend's own greeting; client already received the login-pod greeting.
	if err := discardGreeting(backendRd, s.opts.Protocol); err != nil {
		slog.Debug("login: discard greeting", "err", err)
		return
	}

	// Forward real client IP to backend via protocol-specific XCLIENT.
	if clientIP != "" {
		if err := forwardXClient(backendConn, backendRd, s.opts.Protocol, clientIP, pre.ehloLine); err != nil {
			slog.Debug("login: xclient forward", "proto", s.opts.Protocol, "clientIP", clientIP, "err", err)
			return
		}
	}

	// Replay auth command(s) to backend so it can authenticate the session.
	for _, line := range pre.authLines {
		if _, err := io.WriteString(backendConn, line); err != nil {
			slog.Debug("login: replay auth", "err", err)
			return
		}
	}

	// Discard backend responses to commands the login pod already answered (POP3 USER).
	if err := syncAfterReplay(backendRd, s.opts.Protocol); err != nil {
		slog.Debug("login: sync after replay", "err", err)
		return
	}

	// For Submission: relay backend's AUTH response (235 / 5xx) to the client,
	// then TCP-proxy from MAIL FROM onwards.
	if isSubmission(s.opts.Protocol) {
		line, err := backendRd.ReadString('\n')
		if err != nil {
			slog.Debug("login: read submission auth response", "err", err)
			return
		}
		if _, err := io.WriteString(conn, line); err != nil {
			slog.Debug("login: forward submission auth response", "err", err)
			return
		}
		if len(line) < 3 || line[0] != '2' {
			slog.Debug("login: submission auth rejected by backend", "resp", line)
			return
		}
	}

	conn.SetDeadline(time.Time{})        //nolint:errcheck
	backendConn.SetDeadline(time.Time{}) //nolint:errcheck

	biProxy(rd, conn, backendRd, backendConn)
}

// directorLookup dials yarilo-director, issues a LOOKUP restricted to s.opts.Tag,
// and returns the backend address.
func (s *Server) directorLookup(username string) (string, error) {
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
	result, err := c.Lookup(id, username, s.opts.Tag)
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

// Watch maintains a persistent director connection for receiving USER-KICKED pushes.
// Must be started as a goroutine before serving connections.
func (s *Server) Watch(ctx context.Context) {
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

func discardGreeting(rd *bufio.Reader, p Protocol) error {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		_, err := rd.ReadString('\n')
		return err
	case ProtocolPOP3, ProtocolPOP3S:
		_, err := rd.ReadString('\n')
		return err
	case ProtocolSubmission, ProtocolSubmissions:
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return err
			}
			if len(line) < 4 || line[3] != '-' {
				return nil
			}
		}
	}
	return nil
}

// forwardXClient sends real client IP to backend via protocol-specific XCLIENT.
// For Submission it also replays the EHLO after XCLIENT resets the session.
func forwardXClient(conn net.Conn, rd *bufio.Reader, p Protocol, clientIP, ehloLine string) error {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		if _, err := fmt.Fprintf(conn, "XCONN XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient imap send: %w", err)
		}
		if _, err := rd.ReadString('\n'); err != nil {
			return fmt.Errorf("xclient imap ack: %w", err)
		}
	case ProtocolPOP3, ProtocolPOP3S:
		if _, err := fmt.Fprintf(conn, "XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient pop3 send: %w", err)
		}
		if _, err := rd.ReadString('\n'); err != nil {
			return fmt.Errorf("xclient pop3 ack: %w", err)
		}
	case ProtocolSubmission, ProtocolSubmissions:
		if _, err := fmt.Fprintf(conn, "XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient smtp send: %w", err)
		}
		if _, err := rd.ReadString('\n'); err != nil {
			return fmt.Errorf("xclient smtp ack: %w", err)
		}
		// XCLIENT resets SMTP session; re-send EHLO so the backend can advertise capabilities.
		if ehloLine == "" {
			ehloLine = "EHLO yarilo-submission-login\r\n"
		}
		if _, err := io.WriteString(conn, ehloLine); err != nil {
			return fmt.Errorf("xclient smtp ehlo send: %w", err)
		}
		// Discard multi-line EHLO response (250-... lines).
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return fmt.Errorf("xclient smtp ehlo resp: %w", err)
			}
			if len(line) >= 4 && line[3] != '-' {
				break
			}
		}
	}
	return nil
}

// syncAfterReplay discards backend responses to replayed commands that the login
// pod already answered to the client (POP3 USER response).
func syncAfterReplay(rd *bufio.Reader, p Protocol) error {
	if p == ProtocolPOP3 || p == ProtocolPOP3S {
		_, err := rd.ReadString('\n')
		return err
	}
	return nil
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

func writeProtoError(conn net.Conn, p Protocol, tag, msg string) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		if tag != "" {
			fmt.Fprintf(conn, "%s NO [UNAVAILABLE] %s\r\n", tag, msg) //nolint:errcheck
		} else {
			fmt.Fprintf(conn, "* BYE %s\r\n", msg) //nolint:errcheck
		}
	case ProtocolPOP3, ProtocolPOP3S:
		fmt.Fprintf(conn, "-ERR %s\r\n", msg) //nolint:errcheck
	case ProtocolSubmission, ProtocolSubmissions:
		fmt.Fprintf(conn, "421 4.3.0 %s\r\n", msg) //nolint:errcheck
	}
}

func isSubmission(p Protocol) bool {
	return p == ProtocolSubmission || p == ProtocolSubmissions
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
