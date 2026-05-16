package director

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// ProxyConfig describes one mail protocol listener on the director.
type ProxyConfig struct {
	// Protocol is one of: "imap", "imaps", "pop3", "pop3s", "lmtp".
	Protocol string
	// Addr is the listen address, e.g. ":10993".
	Addr string
	// ExtTLS is the client-facing TLS config (external cert presented to mail clients).
	// Nil means plain TCP (IMAP STARTTLS, POP3, LMTP).
	ExtTLS *tls.Config
	// BackendTLS is the internal mTLS config used when dialling backend pods.
	// Nil means plain TCP to backend (acceptable when a service mesh handles transport).
	BackendTLS *tls.Config
	// BackendPort is the containerPort on the backend pod (same as the pod's listen port).
	// Director dials pod-IP:BackendPort directly, bypassing kube-proxy.
	BackendPort int
	// HAProxy enables PROXY protocol v1/v2 header reading from trusted upstreams.
	// The header is parsed during Accept so conn.RemoteAddr() always reflects the real IP.
	HAProxy            bool
	HAProxyTimeout     time.Duration
	HAProxyTrustedNets []*net.IPNet
	// XClient enables forwarding of the real client IP to the backend via XCLIENT.
	XClient bool
}

// StartProxy starts a mail protocol proxy listener in the background.
// Returns immediately; the listener runs until ctx is cancelled.
func (s *Server) StartProxy(ctx context.Context, cfg ProxyConfig) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("director proxy %s listen %s: %w", cfg.Protocol, cfg.Addr, err)
	}
	if cfg.HAProxy {
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            haProxyPolicy(cfg.HAProxyTrustedNets),
			ReadHeaderTimeout: cfg.HAProxyTimeout,
		}
	}
	slog.Info("director proxy listening", "protocol", cfg.Protocol, "addr", cfg.Addr, "haproxy", cfg.HAProxy)
	go s.runProxy(ctx, ln, cfg)
	return nil
}

func (s *Server) runProxy(ctx context.Context, ln net.Listener, cfg ProxyConfig) {
	defer ln.Close()
	go func() { <-ctx.Done(); ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("director proxy accept", "protocol", cfg.Protocol, "err", err)
			return
		}
		go s.handleProxyConn(conn, cfg)
	}
}

func (s *Server) handleProxyConn(conn net.Conn, cfg ProxyConfig) {
	defer conn.Close()

	// Per-connection deadline covers the preamble phase only.
	conn.SetDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck

	// After proxyproto.Listener wraps the conn, RemoteAddr() already reflects
	// the real client IP from the PROXY header (read eagerly during Accept).
	remote := conn.RemoteAddr().String()
	clientIP, _, err := net.SplitHostPort(remote)
	if err != nil {
		clientIP = remote
	}

	// TLS-terminate client connection for IMAPS / POP3S.
	if cfg.ExtTLS != nil {
		tlsConn := tls.Server(conn, cfg.ExtTLS)
		if err := tlsConn.Handshake(); err != nil {
			slog.Debug("proxy: tls handshake", "proto", cfg.Protocol, "remote", remote, "err", err)
			return
		}
		conn = tlsConn
	}

	rd := bufio.NewReaderSize(conn, 4096)

	// Extract username from the protocol preamble.
	pre, err := extractPreamble(conn, rd, cfg.Protocol)
	if err != nil {
		slog.Debug("proxy: preamble", "proto", cfg.Protocol, "remote", remote, "err", err)
		return
	}

	// Consistent-hash lookup → specific backend pod IP.
	b := s.LookupBackend(pre.username)
	if b == nil {
		slog.Warn("proxy: no backends available", "proto", cfg.Protocol, "user", pre.username)
		writeProxyError(conn, cfg.Protocol, pre.cmdTag, "no backends available")
		return
	}

	backendAddr := net.JoinHostPort(b.IP, strconv.Itoa(cfg.BackendPort))
	s.RecordUser(pre.username, backendAddr)

	slog.Info("proxy: session routed",
		"proto", cfg.Protocol,
		"user", pre.username,
		"remote", remote,
		"backend", backendAddr,
	)

	// Dial backend pod directly (pod IP, not service VIP).
	backendConn, err := dialBackend(backendAddr, cfg.BackendTLS)
	if err != nil {
		slog.Error("proxy: dial backend", "addr", backendAddr, "err", err)
		writeProxyError(conn, cfg.Protocol, pre.cmdTag, "backend unavailable")
		return
	}
	defer backendConn.Close()

	backendRd := bufio.NewReaderSize(backendConn, 4096)

	// Discard backend's own greeting; client already received director's greeting.
	if err := discardGreeting(backendRd, cfg.Protocol); err != nil {
		slog.Debug("proxy: discard greeting", "err", err)
		return
	}

	// Forward real client IP to backend via XCLIENT before auth replay.
	if cfg.XClient && clientIP != "" {
		if err := forwardXClient(backendConn, backendRd, cfg.Protocol, clientIP); err != nil {
			slog.Debug("proxy: xclient forward", "proto", cfg.Protocol, "clientIP", clientIP, "err", err)
			return
		}
	}

	// Replay auth command to backend.
	if _, err := io.WriteString(backendConn, pre.authLine); err != nil {
		slog.Debug("proxy: replay auth", "err", err)
		return
	}

	// Discard backend responses to intermediate commands that the director already
	// answered on behalf of the backend (USER for POP3, LHLO+MAIL FROM for LMTP).
	// Without this, the client would receive duplicate responses.
	if err := syncAfterReplay(backendRd, cfg.Protocol); err != nil {
		slog.Debug("proxy: sync after replay", "err", err)
		return
	}

	// Remove deadline for the session lifetime.
	conn.SetDeadline(time.Time{})        //nolint:errcheck
	backendConn.SetDeadline(time.Time{}) //nolint:errcheck

	// Count session exactly: open when biProxy starts, close when it returns.
	s.sessionOpen(b.IP, cfg.Protocol)
	defer s.sessionClose(b.IP, cfg.Protocol)

	// Bidirectional proxy for the rest of the session.
	biProxy(rd, conn, backendRd, backendConn)
}

// haProxyPolicy returns a proxyproto policy that accepts PROXY headers only
// from trusted upstream IPs and ignores them from all others.
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

// forwardXClient sends an XCLIENT command with the real client IP to the backend
// and reads the acknowledgement response. Must be called after discardGreeting.
func forwardXClient(conn net.Conn, rd *bufio.Reader, protocol, clientIP string) error {
	switch protocol {
	case "imap", "imaps":
		// IMAP XCLIENT: tagged command, backend responds "XCONN OK XCLIENT\r\n".
		if _, err := fmt.Fprintf(conn, "XCONN XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient imap send: %w", err)
		}
		if _, err := rd.ReadString('\n'); err != nil {
			return fmt.Errorf("xclient imap ack: %w", err)
		}
	case "pop3", "pop3s":
		// POP3 XCLIENT: backend responds "+OK XCLIENT ...\r\n".
		if _, err := fmt.Fprintf(conn, "XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient pop3 send: %w", err)
		}
		if _, err := rd.ReadString('\n'); err != nil {
			return fmt.Errorf("xclient pop3 ack: %w", err)
		}
	case "lmtp":
		// LMTP XCLIENT: backend responds "220 2.0.0 OK\r\n".
		if _, err := fmt.Fprintf(conn, "XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient lmtp send: %w", err)
		}
		if _, err := rd.ReadString('\n'); err != nil {
			return fmt.Errorf("xclient lmtp ack: %w", err)
		}
	}
	return nil
}

// syncAfterReplay discards backend responses to intermediate commands that the
// director already answered to the client. Called after writing pre.authLine.
//
// POP3: director answered USER with "+OK"; backend will also emit "+OK" for the
// replayed USER — discard it, let the PASS response reach the client via biProxy.
//
// LMTP: director answered LHLO and MAIL FROM; backend emits responses for all
// three replayed commands — discard LHLO block + MAIL FROM line, let RCPT TO
// response reach the client via biProxy.
func syncAfterReplay(rd *bufio.Reader, protocol string) error {
	switch protocol {
	case "pop3", "pop3s":
		_, err := rd.ReadString('\n') // "+OK" for USER
		return err
	case "lmtp":
		// Discard multi-line LHLO 250 response (lines with "250-"; final "250 ").
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return err
			}
			if len(line) >= 4 && line[3] != '-' {
				break
			}
		}
		// Discard "250 OK" for MAIL FROM.
		_, err := rd.ReadString('\n')
		return err
	default:
		return nil
	}
}

// dialBackend opens a TCP (or mTLS) connection to a backend pod.
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

// discardGreeting reads and discards the backend's greeting banner.
// The client already received the director's synthetic greeting.
func discardGreeting(rd *bufio.Reader, protocol string) error {
	switch protocol {
	case "imap", "imaps":
		// Single line: "* OK ...\r\n"
		_, err := rd.ReadString('\n')
		return err
	case "pop3", "pop3s":
		// Single line: "+OK ...\r\n"
		_, err := rd.ReadString('\n')
		return err
	case "lmtp":
		// Multi-line 220 banner; last line has no '-' after the code.
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return err
			}
			if len(line) < 4 || line[3] != '-' {
				return nil
			}
		}
	default:
		return nil
	}
}

// biProxy copies data in both directions until either side closes.
// clientRd is a bufio.Reader wrapping clientW — it drains its buffer before
// reading from the underlying connection.
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

// writeProxyError sends a protocol-appropriate error to the client.
func writeProxyError(conn net.Conn, protocol, tag, msg string) {
	switch protocol {
	case "imap", "imaps":
		if tag != "" {
			fmt.Fprintf(conn, "%s NO [UNAVAILABLE] %s\r\n", tag, msg) //nolint:errcheck
		} else {
			fmt.Fprintf(conn, "* BYE %s\r\n", msg) //nolint:errcheck
		}
	case "pop3", "pop3s":
		fmt.Fprintf(conn, "-ERR %s\r\n", msg) //nolint:errcheck
	case "lmtp":
		fmt.Fprintf(conn, "421 4.3.0 %s\r\n", msg) //nolint:errcheck
	}
}
