// Package client implements the yarilo-auth TAB-delimited client protocol.
// All public methods are safe for use from a single goroutine; do not share
// a Client across goroutines without external synchronization.
package client

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Sentinel errors returned by Authenticate and Verify.
var (
	ErrAuthFailed = errors.New("auth/client: authentication failed")
	ErrTempFail   = errors.New("auth/client: temporary backend failure")
)

// AuthResult carries the fields from a successful AUTH OK response.
type AuthResult struct {
	Username  string
	Nologin   bool
	AllowNets string
	Token     string
}

// Client is a single persistent connection to yarilo-auth.
type Client struct {
	conn net.Conn
	rd   *bufio.Reader
	seq  int
}

// Dial connects to addr (TCP or Unix socket) and performs the VERSION
// handshake. tlsCfg may be nil for plain (non-TLS) connections.
func Dial(addr string, tlsCfg *tls.Config) (*Client, error) {
	const dialTimeout = 5 * time.Second

	var raw net.Conn
	var err error
	if tlsCfg != nil {
		raw, err = tls.DialWithDialer(&net.Dialer{Timeout: dialTimeout}, "tcp", addr, tlsCfg)
	} else {
		raw, err = net.DialTimeout("tcp", addr, dialTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("auth/client: dial %s: %w", addr, err)
	}

	c := &Client{conn: raw, rd: bufio.NewReader(raw)}
	if err := c.handshake(); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Authenticate sends an AUTH command for the given credentials and returns the
// result. remoteIP and sessionID may be empty strings (omitted from request).
func (c *Client) Authenticate(username, password, service, remoteIP, sessionID string) (*AuthResult, error) {
	c.seq++
	id := fmt.Sprintf("%d", c.seq)

	var sb strings.Builder
	sb.WriteString("AUTH\t")
	sb.WriteString(id)
	sb.WriteString("\tPLAIN")
	sb.WriteString("\tuser=")
	sb.WriteString(username)
	sb.WriteString("\tresp=")
	// SASL PLAIN: NUL + authid + NUL + password
	sb.WriteString("\x00")
	sb.WriteString(username)
	sb.WriteString("\x00")
	sb.WriteString(password)
	if service != "" {
		sb.WriteString("\tservice=")
		sb.WriteString(service)
	}
	if remoteIP != "" {
		sb.WriteString("\trip=")
		sb.WriteString(remoteIP)
	}
	if sessionID != "" {
		sb.WriteString("\tsession=")
		sb.WriteString(sessionID)
	}

	if err := c.writeLine(sb.String()); err != nil {
		return nil, err
	}
	return c.readAuthResponse(id)
}

// Verify sends a VERIFY command and returns the username and session ID bound
// to the token. Returns ErrAuthFailed when the token is unknown or expired.
func (c *Client) Verify(token string) (username, sessionID string, err error) {
	c.seq++
	id := fmt.Sprintf("%d", c.seq)

	if err := c.writeLine(fmt.Sprintf("VERIFY\t%s\t%s", id, token)); err != nil {
		return "", "", err
	}
	return c.readVerifyResponse(id)
}

// handshake exchanges VERSION lines.
func (c *Client) handshake() error {
	if err := c.writeLine("VERSION\t1\t0"); err != nil {
		return err
	}
	gotVersion := false
	for {
		line, err := c.readLine()
		if err != nil {
			return fmt.Errorf("auth/client: handshake read: %w", err)
		}
		if strings.HasPrefix(line, "VERSION\t") {
			gotVersion = true
			continue
		}
		if line == "DONE" {
			break
		}
		// MECH, SPID, CUID, COOKIE — skip
	}
	if !gotVersion {
		return fmt.Errorf("auth/client: handshake: no VERSION received")
	}
	return nil
}

func (c *Client) writeLine(s string) error {
	_, err := fmt.Fprintln(c.conn, s)
	if err != nil {
		return fmt.Errorf("auth/client: write: %w", err)
	}
	return nil
}

func (c *Client) readLine() (string, error) {
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("auth/client: read: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *Client) readAuthResponse(id string) (*AuthResult, error) {
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return nil, fmt.Errorf("auth/client: malformed response: %q", line)
	}
	if fields[1] != id {
		return nil, fmt.Errorf("auth/client: response id mismatch: got %q want %q", fields[1], id)
	}
	switch fields[0] {
	case "OK":
		res := &AuthResult{}
		for _, f := range fields[2:] {
			switch {
			case f == "nologin":
				res.Nologin = true
			case strings.HasPrefix(f, "user="):
				res.Username = f[len("user="):]
			case strings.HasPrefix(f, "allow_nets="):
				res.AllowNets = f[len("allow_nets="):]
			case strings.HasPrefix(f, "token="):
				res.Token = f[len("token="):]
			}
		}
		return res, nil
	case "FAIL":
		for _, f := range fields[2:] {
			if f == "temp_fail" {
				return nil, ErrTempFail
			}
		}
		return nil, ErrAuthFailed
	default:
		return nil, fmt.Errorf("auth/client: unknown response verb %q", fields[0])
	}
}

func (c *Client) readVerifyResponse(id string) (string, string, error) {
	line, err := c.readLine()
	if err != nil {
		return "", "", err
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 2 {
		return "", "", fmt.Errorf("auth/client: malformed verify response: %q", line)
	}
	if fields[1] != id {
		return "", "", fmt.Errorf("auth/client: verify id mismatch: got %q want %q", fields[1], id)
	}
	switch fields[0] {
	case "OK":
		var username, sessionID string
		for _, f := range fields[2:] {
			switch {
			case strings.HasPrefix(f, "user="):
				username = f[len("user="):]
			case strings.HasPrefix(f, "session="):
				sessionID = f[len("session="):]
			}
		}
		return username, sessionID, nil
	case "FAIL":
		return "", "", ErrAuthFailed
	default:
		return "", "", fmt.Errorf("auth/client: unknown verify verb %q", fields[0])
	}
}
