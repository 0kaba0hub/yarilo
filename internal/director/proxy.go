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
}

// StartProxy starts a mail protocol proxy listener in the background.
// Returns immediately; the listener runs until ctx is cancelled.
func (s *Server) StartProxy(ctx context.Context, cfg ProxyConfig) error {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("director proxy %s listen %s: %w", cfg.Protocol, cfg.Addr, err)
	}
	slog.Info("director proxy listening", "protocol", cfg.Protocol, "addr", cfg.Addr)
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

	remote := conn.RemoteAddr().String()

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

	// Replay auth command to backend.
	if _, err := io.WriteString(backendConn, pre.authLine); err != nil {
		slog.Debug("proxy: replay auth", "err", err)
		return
	}

	// Remove deadline for the session lifetime.
	conn.SetDeadline(time.Time{})        //nolint:errcheck
	backendConn.SetDeadline(time.Time{}) //nolint:errcheck

	// Bidirectional proxy for the rest of the session.
	biProxy(rd, conn, backendRd, backendConn)
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
