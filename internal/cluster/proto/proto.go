// Package proto implements the yarilo-director TAB-delimited wire protocol.
// In single-node mode this package is not used (director is in-process).
package proto

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ErrLookupHold is returned when the director holds LOOKUP for a user
// under a confirmed ring-wide kick. Retryable: re-LOOKUP shortly.
var ErrLookupHold = errors.New("director lookup: user kill in progress, retry")

// lookupHoldReason is the FAIL reason the director sends for the hold.
const lookupHoldReason = "killing"

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

// readReply reads the next reply line, skipping unsolicited server
// pushes (RING-CHANGE / USER-* / PING keepalives) that the director
// fans out to every connection, including mid-request ones. PING is
// answered with PONG.
func (c *Conn) readReply() (string, error) {
	for {
		line, err := c.ReadLine()
		if err != nil {
			return "", err
		}
		switch replyVerb(line) {
		case "RING-CHANGE", "USER-MOVED", "USER-KICKED", "USER-KILLED-EVERYWHERE":
			continue
		case "PING":
			_ = c.WriteLine("PONG")
			continue
		}
		return line, nil
	}
}

func replyVerb(line string) string {
	if i := strings.IndexByte(line, '\t'); i >= 0 {
		return line[:i]
	}
	return line
}

// Lookup asks the director for the backend address for username. tag
// restricts routing to that tag's pool; "" is the untagged pool — there
// is no full-ring mode, every caller belongs to exactly one tag-pool.
func (c *Conn) Lookup(id, username, tag, proto string) (LookupResult, error) {
	// trailing proto field feeds least_sessions placement; optional
	// for older callers
	if err := c.WriteLine(LookupRequestLine(id, username, tag, proto)); err != nil {
		return LookupResult{}, fmt.Errorf("director lookup: write: %w", err)
	}
	line, err := c.readReply()
	if err != nil {
		return LookupResult{}, fmt.Errorf("director lookup: read: %w", err)
	}
	return ParseLookupReply(line)
}

// LookupRequestLine renders a LOOKUP request, for callers that own a
// persistent director connection and match replies by id.
func LookupRequestLine(id, username, tag, proto string) string {
	return fmt.Sprintf("LOOKUP\t%s\t%s\t%s\t%s", id, TabEscape(username), tag, proto)
}

// ParseLookupReply decodes a HOST or FAIL reply to a LOOKUP.
func ParseLookupReply(line string) (LookupResult, error) {
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
		reason := fields[len(fields)-1]
		// confirmed-kick hold is retryable: the user's old sessions
		// are still draining, re-LOOKUP instead of erroring the client
		if reason == "reason="+lookupHoldReason {
			return LookupResult{}, fmt.Errorf("director lookup: %w", ErrLookupHold)
		}
		return LookupResult{}, fmt.Errorf("director lookup: %s", reason)
	default:
		return LookupResult{}, fmt.Errorf("director lookup: unknown response: %q", line)
	}
}

// SessionOpen registers an active proxied session with the director.
// sessionID is a login-pod-local unique ID; backendIP is the IP of the backend serving the session.
// The director uses this to send USER-KICKED when the backend goes down.
func (c *Conn) SessionOpen(sessionID, username, backendIP, proto string) error {
	if err := c.WriteLine(fmt.Sprintf("SESSION-OPEN\t%s\t%s\t%s\t%s", sessionID, TabEscape(username), backendIP, proto)); err != nil {
		return fmt.Errorf("director session-open: write: %w", err)
	}
	line, err := c.readReply()
	if err != nil {
		return fmt.Errorf("director session-open: read: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("director session-open: unexpected response: %q", line)
	}
	return nil
}

// Unreachable reports a failed dial to backendIP. Corroborated across
// enough login proxies, this evicts the backend ahead of its lease TTL.
func (c *Conn) Unreachable(backendIP string) error {
	if err := c.WriteLine("BACKEND-UNREACHABLE\t" + backendIP); err != nil {
		return fmt.Errorf("director unreachable: write: %w", err)
	}
	line, err := c.readReply()
	if err != nil {
		return fmt.Errorf("director unreachable: read: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("director unreachable: unexpected response: %q", line)
	}
	return nil
}

// SessionClose unregisters a proxied session from the director.
func (c *Conn) SessionClose(sessionID string) error {
	if err := c.WriteLine(fmt.Sprintf("SESSION-CLOSE\t%s", sessionID)); err != nil {
		return fmt.Errorf("director session-close: write: %w", err)
	}
	line, err := c.readReply()
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
	line, err := c.readReply()
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
	line, err := c.readReply()
	if err != nil {
		return fmt.Errorf("director backend-down: read: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("director backend-down: unexpected response: %q", line)
	}
	return nil
}

// ReadLine reads one LF-terminated line, capped at maxLineLen.
// ReadSlice fails with ErrBufferFull once the buffer fills, so a peer
// streaming bytes with no LF costs at most one buffer.
func (c *Conn) ReadLine() (string, error) {
	line, err := c.rd.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("cluster/proto: line exceeds %d bytes", maxLineLen)
		}
		return "", err
	}
	return strings.TrimRight(string(line), "\n"), nil
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
