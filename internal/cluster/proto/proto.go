// Package proto implements the yarilo-director TAB-delimited wire protocol.
// In single-node mode this package is not used (director is in-process).
package proto

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	protoName  = "yarilo-director"
	majorVer   = 1
	minorVer   = 0
	maxLineLen = 16384
)

// Conn wraps a net.Conn with line-oriented TAB-delimited framing.
type Conn struct {
	conn net.Conn
	rd   *bufio.Reader
}

// Dial connects to a director over plain TCP and performs the client handshake.
func Dial(addr, localIP string, localPort int) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return newConn(nc, localIP, localPort)
}

// DialTLS connects to a director over mTLS and performs the client handshake.
func DialTLS(addr, localIP string, localPort int, tlsCfg *tls.Config) (*Conn, error) {
	nc, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, tlsCfg)
	if err != nil {
		return nil, err
	}
	return newConn(nc, localIP, localPort)
}

func newConn(nc net.Conn, localIP string, localPort int) (*Conn, error) {
	c := &Conn{conn: nc, rd: bufio.NewReaderSize(nc, maxLineLen)}
	if err := c.readServerHandshake(); err != nil {
		nc.Close()
		return nil, err
	}
	if err := c.sendHandshake(localIP, localPort); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) readServerHandshake() error {
	for {
		line, err := c.ReadLine()
		if err != nil {
			return fmt.Errorf("director handshake: %w", err)
		}
		if line == "DONE" {
			return nil
		}
	}
}

func (c *Conn) sendHandshake(ip string, localPort int) error {
	if err := c.WriteLine(fmt.Sprintf("VERSION\t%s\t%d\t%d", protoName, majorVer, minorVer)); err != nil {
		return err
	}
	ts := time.Now().Unix()
	if err := c.WriteLine(fmt.Sprintf("ME\t%s\t%d\t%d", ip, localPort, ts)); err != nil {
		return err
	}
	return c.WriteLine("DONE")
}

// LookupResult is the response from a successful LOOKUP.
type LookupResult struct {
	Addr string // "ip:port"
	Tag  string // backend tag (may be empty)
}

// Lookup asks the director for the backend address for the given username.
// tag restricts routing to backends with that tag; pass "" for the full ring.
// Returns LookupResult on success, or an error if no backends are available.
func (c *Conn) Lookup(id, username, tag string) (LookupResult, error) {
	if err := c.WriteLine(fmt.Sprintf("LOOKUP\t%s\t%s\t%s", id, TabEscape(username), tag)); err != nil {
		return LookupResult{}, fmt.Errorf("director lookup: write: %w", err)
	}
	line, err := c.ReadLine()
	if err != nil {
		return LookupResult{}, fmt.Errorf("director lookup: read: %w", err)
	}
	fields := ParseLine(line)
	if len(fields) < 2 {
		return LookupResult{}, fmt.Errorf("director lookup: unexpected response: %q", line)
	}
	switch fields[0] {
	case "HOST":
		// HOST\t{id}\t{ip}\t{port}\t{tag}  (tag optional)
		if len(fields) < 4 {
			return LookupResult{}, fmt.Errorf("director lookup: malformed HOST: %q", line)
		}
		tag := ""
		if len(fields) >= 5 {
			tag = fields[4]
		}
		return LookupResult{
			Addr: net.JoinHostPort(fields[2], fields[3]),
			Tag:  tag,
		}, nil
	case "FAIL":
		return LookupResult{}, fmt.Errorf("director lookup: %s", fields[len(fields)-1])
	default:
		return LookupResult{}, fmt.Errorf("director lookup: unknown response: %q", line)
	}
}

// SessionOpen registers an active proxied session with the director.
// sessionID is a login-pod-local unique ID; backendIP is the IP of the backend serving the session.
// The director uses this to send USER-KICKED when the backend goes down.
func (c *Conn) SessionOpen(sessionID, username, backendIP string) error {
	if err := c.WriteLine(fmt.Sprintf("SESSION-OPEN\t%s\t%s\t%s", sessionID, TabEscape(username), backendIP)); err != nil {
		return fmt.Errorf("director session-open: write: %w", err)
	}
	line, err := c.ReadLine()
	if err != nil {
		return fmt.Errorf("director session-open: read: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("director session-open: unexpected response: %q", line)
	}
	return nil
}

// SessionClose unregisters a proxied session from the director.
func (c *Conn) SessionClose(sessionID string) error {
	if err := c.WriteLine(fmt.Sprintf("SESSION-CLOSE\t%s", sessionID)); err != nil {
		return fmt.Errorf("director session-close: write: %w", err)
	}
	line, err := c.ReadLine()
	if err != nil {
		return fmt.Errorf("director session-close: read: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("director session-close: unexpected response: %q", line)
	}
	return nil
}

// BackendUp registers or marks a backend as available.
func (c *Conn) BackendUp(ip string, port int, tag string) error {
	if err := c.WriteLine(fmt.Sprintf("BACKEND-UP\t%s\t%d\t%s", ip, port, tag)); err != nil {
		return fmt.Errorf("director backend-up: write: %w", err)
	}
	line, err := c.ReadLine()
	if err != nil {
		return fmt.Errorf("director backend-up: read: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("director backend-up: unexpected response: %q", line)
	}
	return nil
}

// BackendDown removes a backend from the ring.
func (c *Conn) BackendDown(ip string) error {
	if err := c.WriteLine(fmt.Sprintf("BACKEND-DOWN\t%s", ip)); err != nil {
		return fmt.Errorf("director backend-down: write: %w", err)
	}
	line, err := c.ReadLine()
	if err != nil {
		return fmt.Errorf("director backend-down: read: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("director backend-down: unexpected response: %q", line)
	}
	return nil
}

// ReadLine reads one LF-terminated line (max 16384 bytes).
func (c *Conn) ReadLine() (string, error) {
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\n"), nil
}

// WriteLine writes a line followed by LF.
func (c *Conn) WriteLine(line string) error {
	_, err := io.WriteString(c.conn, line+"\n")
	return err
}

// ParseLine splits a TAB-delimited line into fields.
func ParseLine(line string) []string {
	return strings.Split(line, "\t")
}

// Close closes the underlying connection.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// TabEscape escapes special characters for TAB-delimited transmission.
// Mirrors Dovecot str_tabescape.
func TabEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// TabUnescape reverses TabEscape.
func TabUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
