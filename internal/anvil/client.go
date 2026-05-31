package anvil

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Conn is a single TCP connection to yarilo-anvil for one login session.
// Dial, call Connect once, defer Disconnect+Close.
type Conn struct {
	conn net.Conn
	rd   *bufio.Reader
}

// Dial connects to the anvil server, reads the version handshake, and returns
// a ready Conn. tlsCfg may be nil for plain TCP.
func Dial(addr string, tlsCfg *tls.Config, timeout time.Duration) (*Conn, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	var raw net.Conn
	var err error
	if tlsCfg != nil {
		raw, err = tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr, tlsCfg)
	} else {
		raw, err = net.DialTimeout("tcp", addr, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("anvil/client: dial %s: %w", addr, err)
	}
	c := &Conn{conn: raw, rd: bufio.NewReaderSize(raw, 512)}
	if err := c.readHandshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) readHandshake() error {
	// VERSION\tyarilo-anvil\t1\t0\n
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("anvil/client: read version: %w", err)
	}
	line = strings.TrimRight(line, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || fields[0] != "VERSION" || fields[1] != protoName {
		return fmt.Errorf("anvil/client: unexpected handshake: %q", line)
	}
	// DONE\n
	done, err := c.rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("anvil/client: read done: %w", err)
	}
	if strings.TrimRight(done, "\n") != "DONE" {
		return fmt.Errorf("anvil/client: expected DONE, got %q", done)
	}
	return nil
}

// Connect sends CONNECT and returns nil on OK, ErrTooManyConns on FAIL, or a
// transport error. id must be unique per request (e.g. session sequence number).
func (c *Conn) Connect(id, user, ip, service string) error {
	if _, err := fmt.Fprintf(c.conn, "CONNECT\t%s\t%s\t%s\t%s\n", id, user, ip, service); err != nil {
		return fmt.Errorf("anvil/client: write CONNECT: %w", err)
	}
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("anvil/client: read CONNECT response: %w", err)
	}
	line = strings.TrimRight(line, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) >= 1 && fields[0] == "FAIL" {
		return ErrTooManyConns
	}
	if len(fields) < 2 || fields[0] != "OK" {
		return fmt.Errorf("anvil/client: unexpected CONNECT response: %q", line)
	}
	return nil
}

// Disconnect sends DISCONNECT and reads the acknowledgement.
// Errors are non-fatal (session is ending anyway).
func (c *Conn) Disconnect(id, user, ip, service string) error {
	if _, err := fmt.Fprintf(c.conn, "DISCONNECT\t%s\t%s\t%s\t%s\n", id, user, ip, service); err != nil {
		return fmt.Errorf("anvil/client: write DISCONNECT: %w", err)
	}
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("anvil/client: read DISCONNECT response: %w", err)
	}
	line = strings.TrimRight(line, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) < 1 || fields[0] != "OK" {
		return fmt.Errorf("anvil/client: unexpected DISCONNECT response: %q", line)
	}
	return nil
}

// WhoFilter narrows the WHO listing. Empty fields match everything.
type WhoFilter struct {
	Service string
	User    string
}

// Who streams the active session list from the anvil server. The
// returned slice is empty (not nil) when no sessions match. Errors
// are transport-level — an empty list with no error means the
// server returned DONE without any sessions.
func (c *Conn) Who(f WhoFilter) ([]SessionInfo, error) {
	args := []string{"WHO"}
	if f.Service != "" {
		args = append(args, "service="+f.Service)
	}
	if f.User != "" {
		args = append(args, "user="+f.User)
	}
	if _, err := fmt.Fprintln(c.conn, strings.Join(args, "\t")); err != nil {
		return nil, fmt.Errorf("anvil/client: write WHO: %w", err)
	}
	out := make([]SessionInfo, 0, 8)
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("anvil/client: read WHO response: %w", err)
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "SESSION":
			if len(fields) < 6 {
				continue
			}
			ts, _ := strconv.ParseInt(fields[5], 10, 64)
			out = append(out, SessionInfo{
				ID:          fields[1],
				User:        fields[2],
				IP:          fields[3],
				Service:     fields[4],
				ConnectedAt: time.Unix(ts, 0).UTC(),
			})
		case "DONE":
			return out, nil
		default:
			return nil, fmt.Errorf("anvil/client: unexpected WHO line: %q", line)
		}
	}
}

// Close closes the underlying TCP connection.
func (c *Conn) Close() { c.conn.Close() }

// ErrTooManyConns is returned by Connect when the anvil server responds FAIL
// with reason=too-many-connections.
var ErrTooManyConns = fmt.Errorf("anvil: too many connections")
