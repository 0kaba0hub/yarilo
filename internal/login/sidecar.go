package login

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

// SidecarOptions configures the backend login sidecar.
// The sidecar runs as a container in the same pod as the session process
// (yarilo-imap, yarilo-pop3, yarilo-submission). It accepts connections from
// frontend login pods, receives the real client IP via XCLIENT, then connects
// to the local session process and proxies the session.
type SidecarOptions struct {
	Protocol Protocol

	// ExtTLS is the mTLS server config for accepting connections from frontend
	// login pods. Nil = plain TCP (for cluster-internal connections).
	ExtTLS *tls.Config

	// SessionAddr is the local address of the session process.
	// e.g. "127.0.0.1:10993" for yarilo-imap.
	SessionAddr string

	// SessionTLS is the TLS config for connecting to the session process.
	// Nil = plain TCP.
	SessionTLS *tls.Config
}

// Sidecar is the backend login sidecar server.
type Sidecar struct {
	opts SidecarOptions
}

// NewSidecar creates a Sidecar.
func NewSidecar(opts SidecarOptions) *Sidecar {
	return &Sidecar{opts: opts}
}

// Serve accepts connections on ln until the listener is closed.
func (s *Sidecar) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("sidecar: accept: %w", err)
		}
		go s.handleConn(conn)
	}
}

func (s *Sidecar) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck

	remote := conn.RemoteAddr().String()

	// mTLS upgrade for connections from frontend login pods.
	if s.opts.ExtTLS != nil {
		tlsConn := tls.Server(conn, s.opts.ExtTLS)
		if err := tlsConn.Handshake(); err != nil {
			slog.Debug("sidecar: tls handshake", "proto", s.opts.Protocol, "remote", remote, "err", err)
			return
		}
		conn = tlsConn
	}

	// Send greeting so the frontend login pod can discard it consistently.
	if err := sidecarSendGreeting(conn, s.opts.Protocol); err != nil {
		slog.Debug("sidecar: send greeting", "err", err)
		return
	}

	rd := bufio.NewReaderSize(conn, 4096)

	// Read XCLIENT from frontend and ACK it; extract real client IP.
	clientIP, err := sidecarRecvXClient(conn, rd, s.opts.Protocol)
	if err != nil {
		slog.Debug("sidecar: recv xclient", "proto", s.opts.Protocol, "remote", remote, "err", err)
		return
	}

	// Connect to local session process.
	sessionConn, err := dialBackend(s.opts.SessionAddr, s.opts.SessionTLS)
	if err != nil {
		slog.Error("sidecar: dial session", "addr", s.opts.SessionAddr, "err", err)
		return
	}
	defer sessionConn.Close()

	sessionRd := bufio.NewReaderSize(sessionConn, 4096)

	// Discard session greeting; the frontend already sent its own greeting to the client.
	if err := discardGreeting(sessionRd, s.opts.Protocol); err != nil {
		slog.Debug("sidecar: discard session greeting", "err", err)
		return
	}

	// Forward real client IP to session via XCLIENT.
	// For SMTP, omit the EHLO relay — it flows through biProxy from the frontend.
	if clientIP != "" {
		if err := sidecarForwardXClient(sessionConn, sessionRd, s.opts.Protocol, clientIP); err != nil {
			slog.Debug("sidecar: forward xclient to session", "err", err)
			return
		}
	}

	slog.Info("sidecar: session connected",
		"proto", s.opts.Protocol,
		"remote", remote,
		"client_ip", clientIP,
		"session", s.opts.SessionAddr,
	)

	conn.SetDeadline(time.Time{})        //nolint:errcheck
	sessionConn.SetDeadline(time.Time{}) //nolint:errcheck

	// biProxy: auth replay and all session commands flow transparently.
	biProxy(rd, conn, sessionRd, sessionConn)
}

// sidecarSendGreeting sends a protocol greeting that the frontend will discard.
func sidecarSendGreeting(conn net.Conn, p Protocol) error {
	var line string
	switch {
	case p == ProtocolIMAP || p == ProtocolIMAPS:
		line = "* OK yarilo-login ready\r\n"
	case p == ProtocolPOP3 || p == ProtocolPOP3S:
		line = "+OK yarilo-login ready\r\n"
	case isSubmission(p):
		line = "220 yarilo-login ready\r\n"
	default:
		return fmt.Errorf("sidecar: unknown protocol %q", p)
	}
	_, err := fmt.Fprint(conn, line)
	return err
}

// sidecarRecvXClient reads the XCLIENT line sent by the frontend login pod,
// sends the protocol-appropriate ACK, and returns the real client IP.
func sidecarRecvXClient(conn net.Conn, rd *bufio.Reader, p Protocol) (string, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read xclient line: %w", err)
	}
	clean := strings.TrimRight(line, "\r\n")
	fields := strings.Fields(clean)

	// Extract ADDR=value from any field.
	ip := ""
	for _, f := range fields {
		if strings.HasPrefix(strings.ToUpper(f), "ADDR=") {
			ip = f[5:]
		}
	}

	switch {
	case p == ProtocolIMAP || p == ProtocolIMAPS:
		// Frontend sends: "XCONN XCLIENT ADDR=ip" — tag is fields[0].
		tag := "XCONN"
		if len(fields) > 0 {
			tag = fields[0]
		}
		if _, err := fmt.Fprintf(conn, "%s OK XCLIENT\r\n", tag); err != nil {
			return "", fmt.Errorf("ack xclient imap: %w", err)
		}
	case p == ProtocolPOP3 || p == ProtocolPOP3S:
		if _, err := fmt.Fprintf(conn, "+OK XCLIENT accepted\r\n"); err != nil {
			return "", fmt.Errorf("ack xclient pop3: %w", err)
		}
	case isSubmission(p):
		if _, err := fmt.Fprintf(conn, "250 OK\r\n"); err != nil {
			return "", fmt.Errorf("ack xclient smtp: %w", err)
		}
	}
	return ip, nil
}

// sidecarForwardXClient sends XCLIENT to the local session process.
// Unlike forwardXClient, it does NOT send EHLO for SMTP — the EHLO flows
// through biProxy from the frontend's own forwardXClient call.
func sidecarForwardXClient(conn net.Conn, rd *bufio.Reader, p Protocol, clientIP string) error {
	switch {
	case p == ProtocolIMAP || p == ProtocolIMAPS:
		if _, err := fmt.Fprintf(conn, "XCONN XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient imap send: %w", err)
		}
		_, err := rd.ReadString('\n')
		return err
	case p == ProtocolPOP3 || p == ProtocolPOP3S:
		if _, err := fmt.Fprintf(conn, "XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient pop3 send: %w", err)
		}
		_, err := rd.ReadString('\n')
		return err
	case isSubmission(p):
		if _, err := fmt.Fprintf(conn, "XCLIENT ADDR=%s\r\n", clientIP); err != nil {
			return fmt.Errorf("xclient smtp send: %w", err)
		}
		_, err := rd.ReadString('\n')
		return err
	}
	return nil
}
