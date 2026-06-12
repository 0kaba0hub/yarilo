package loginproto

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"time"

	authclient "github.com/0kaba0hub/yarilo/internal/auth/client"
)

// PreambleConn wraps a net.Conn after the YARILO preamble has been read.
// It exposes the preamble fields and overrides RemoteAddr with the real
// client IP forwarded by the login pod.
//
// Read is proxied through the buffered reader that was used to read the
// preamble so no bytes are lost.
type PreambleConn struct {
	net.Conn
	Username  string
	SessionID string
	Helo      string
	realAddr  net.Addr
	br        *bufio.Reader
}

// RemoteAddr returns the real client IP forwarded in the preamble.
func (c *PreambleConn) RemoteAddr() net.Addr { return c.realAddr }

// Read reads from the buffered reader (preserving any bytes already buffered
// after the preamble line).
func (c *PreambleConn) Read(b []byte) (int, error) { return c.br.Read(b) }

// PreambleListener wraps a net.Listener. Accept reads the YARILO preamble from
// each incoming connection, calls yarilo-auth VERIFY to validate the session
// token, and returns a *PreambleConn. Connections that fail preamble reading or
// token verification are closed and skipped (Accept loops to the next connection).
type PreambleListener struct {
	net.Listener
	AuthAddr string
	AuthTLS  *tls.Config
}

const preambleReadTimeout = 5 * time.Second

// Accept accepts the next connection, reads the YARILO preamble, verifies the
// session token with yarilo-auth, and returns a *PreambleConn on success.
func (l *PreambleListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		pc, err := l.handshake(c)
		if err != nil {
			slog.Debug("loginproto: preamble handshake failed", "remote", c.RemoteAddr(), "err", err)
			c.Close()
			continue
		}
		return pc, nil
	}
}

func (l *PreambleListener) handshake(c net.Conn) (*PreambleConn, error) {
	c.SetDeadline(time.Now().Add(preambleReadTimeout)) //nolint:errcheck

	br := bufio.NewReader(c)
	pre, err := Parse(br)
	if err != nil {
		return nil, fmt.Errorf("preamble read: %w", err)
	}

	c.SetDeadline(time.Time{}) //nolint:errcheck

	authCl, err := authclient.Dial(l.AuthAddr, l.AuthTLS)
	if err != nil {
		return nil, fmt.Errorf("auth dial: %w", err)
	}
	defer authCl.Close()

	username, sessionID, err := authCl.Verify(pre.Token)
	if err != nil {
		return nil, fmt.Errorf("token verify: %w", err)
	}
	if username == "" {
		return nil, fmt.Errorf("token verify: empty username")
	}

	var realAddr net.Addr = c.RemoteAddr()
	if pre.Addr != "" {
		if ip := net.ParseIP(pre.Addr); ip != nil {
			realAddr = &net.TCPAddr{IP: ip}
		}
	}

	_ = sessionID // sessionID from preamble and from VERIFY both valid; preamble's is used for anvil events

	return &PreambleConn{
		Conn:      c,
		Username:  username,
		SessionID: pre.SessionID,
		Helo:      pre.Helo,
		realAddr:  realAddr,
		br:        br,
	}, nil
}
