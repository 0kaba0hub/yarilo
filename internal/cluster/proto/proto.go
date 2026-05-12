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

// Dial connects to a director and performs the outgoing handshake.
func Dial(addr, localIP string, localPort int) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Conn{conn: nc, rd: bufio.NewReaderSize(nc, maxLineLen)}
	if err := c.sendHandshake(localIP, localPort); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) sendHandshake(ip string, port int) error {
	if err := c.WriteLine(fmt.Sprintf("VERSION\t%s\t%d\t%d", protoName, majorVer, minorVer)); err != nil {
		return err
	}
	ts := time.Now().Unix()
	if err := c.WriteLine(fmt.Sprintf("ME\t%s\t%d\t%d", ip, port, ts)); err != nil {
		return err
	}
	return c.WriteLine("DONE")
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
