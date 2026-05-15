// Package proto implements the yarilo-director TAB-delimited wire protocol.
// In single-node mode this package is not used (director is in-process).
package proto

import (
	"bufio"
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

// Dial connects to a director and performs the client handshake.
// Reads the server's VERSION+DONE before sending VERSION+ME+DONE.
func Dial(addr, localIP string, localPort int) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
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

// Lookup asks the director for the backend address for the given username.
// Returns the backend host:port string, or an error if no backends are available.
func (c *Conn) Lookup(id, username string) (string, error) {
	if err := c.WriteLine(fmt.Sprintf("LOOKUP\t%s\t%s", id, TabEscape(username))); err != nil {
		return "", fmt.Errorf("director lookup: write: %w", err)
	}
	line, err := c.ReadLine()
	if err != nil {
		return "", fmt.Errorf("director lookup: read: %w", err)
	}
	fields := ParseLine(line)
	if len(fields) < 2 {
		return "", fmt.Errorf("director lookup: unexpected response: %q", line)
	}
	switch fields[0] {
	case "HOST":
		if len(fields) < 4 {
			return "", fmt.Errorf("director lookup: malformed HOST: %q", line)
		}
		return net.JoinHostPort(fields[2], fields[3]), nil
	case "FAIL":
		return "", fmt.Errorf("director lookup: %s", fields[len(fields)-1])
	default:
		return "", fmt.Errorf("director lookup: unknown response: %q", line)
	}
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
