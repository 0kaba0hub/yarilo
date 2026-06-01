package anvil

import (
	"bufio"
	"context"
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
			// Folder is the 1.4 addition; tolerate its absence
			// so a 1.3 server (or pre-SELECT session) still
			// parses cleanly.
			var folder string
			if len(fields) >= 7 {
				folder = fields[6]
			}
			out = append(out, SessionInfo{
				ID:          fields[1],
				User:        fields[2],
				IP:          fields[3],
				Service:     fields[4],
				ConnectedAt: time.Unix(ts, 0).UTC(),
				Folder:      folder,
			})
		case "DONE":
			return out, nil
		default:
			return nil, fmt.Errorf("anvil/client: unexpected WHO line: %q", line)
		}
	}
}

// Heartbeat extends the TTL of an active session on the anvil
// server. Returns (true, nil) on hit, (false, nil) when the
// server reports `reason=unknown` (caller should re-issue
// Connect), or an error on transport failure.
func (c *Conn) Heartbeat(id string) (bool, error) {
	if _, err := fmt.Fprintf(c.conn, "HEARTBEAT\t%s\n", id); err != nil {
		return false, fmt.Errorf("anvil/client: write HEARTBEAT: %w", err)
	}
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("anvil/client: read HEARTBEAT response: %w", err)
	}
	line = strings.TrimRight(line, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || fields[0] != "OK" {
		return false, fmt.Errorf("anvil/client: unexpected HEARTBEAT response: %q", line)
	}
	for _, f := range fields[2:] {
		if f == "reason=unknown" {
			return false, nil
		}
	}
	return true, nil
}

// Select tells the anvil server which IMAP mailbox the session
// is currently SELECTed in. Empty folder means UNSELECT.
//
// Best-effort: returns nil for unknown session (the server's
// reason=unknown reply is expected when the session was reaped),
// and surfaces only transport errors. Callers are not expected
// to alter their flow based on the result.
func (c *Conn) Select(id, folder string) error {
	if _, err := fmt.Fprintf(c.conn, "SELECT\t%s\t%s\n", id, folder); err != nil {
		return fmt.Errorf("anvil/client: write SELECT: %w", err)
	}
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("anvil/client: read SELECT response: %w", err)
	}
	line = strings.TrimRight(line, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || fields[0] != "OK" {
		return fmt.Errorf("anvil/client: unexpected SELECT response: %q", line)
	}
	return nil
}

// Lookup asks the server how many active sessions exist for
// (user, service). Returns 0 when no sessions match. Used by
// LMTP at RCPT TO to enforce lmtp_user_concurrency_limit
// cluster-wide before issuing Connect.
func (c *Conn) Lookup(user, service string) (int, error) {
	if _, err := fmt.Fprintf(c.conn, "LOOKUP\t%s\t%s\n", user, service); err != nil {
		return 0, fmt.Errorf("anvil/client: write LOOKUP: %w", err)
	}
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("anvil/client: read LOOKUP response: %w", err)
	}
	line = strings.TrimRight(line, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || fields[0] != "COUNT" {
		return 0, fmt.Errorf("anvil/client: unexpected LOOKUP response: %q", line)
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("anvil/client: bad LOOKUP count %q: %w", fields[1], err)
	}
	return n, nil
}

// Close closes the underlying TCP connection.
func (c *Conn) Close() { c.conn.Close() }

// Emit publishes an event on the named channel. Subscribers
// listening on the same channel receive a push asynchronously.
// Returns nil on the server's OK ack; transport errors propagate.
//
// EMIT is fire-and-forget from a delivery standpoint — the server
// does not block on slow subscribers, so OK means "queued", not
// "received by every subscriber".
func (c *Conn) Emit(channel, payload string) error {
	if _, err := fmt.Fprintf(c.conn, "EMIT\t%s\t%s\n", channel, payload); err != nil {
		return fmt.Errorf("anvil/client: write EMIT: %w", err)
	}
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return fmt.Errorf("anvil/client: read EMIT response: %w", err)
	}
	if strings.TrimRight(line, "\n") != "OK" {
		return fmt.Errorf("anvil/client: unexpected EMIT response: %q", line)
	}
	return nil
}

// Subscribe takes over this connection as a server→client push
// channel for `channel`. The returned chan of payloads receives
// one entry per EVENT line from the server, and closes when ctx
// is cancelled or the underlying conn errors out.
//
// IMPORTANT: SUBSCRIBE makes the connection unusable for any
// other command afterwards — open a dedicated conn for each
// subscription. Caller should not call Close() before ctx is
// cancelled; the subscriber goroutine owns the conn lifecycle.
func (c *Conn) Subscribe(ctx context.Context, channel string) (<-chan string, error) {
	if _, err := fmt.Fprintf(c.conn, "SUBSCRIBE\t%s\n", channel); err != nil {
		return nil, fmt.Errorf("anvil/client: write SUBSCRIBE: %w", err)
	}
	line, err := c.rd.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("anvil/client: read SUBSCRIBE ack: %w", err)
	}
	if strings.TrimRight(line, "\n") != "OK" {
		return nil, fmt.Errorf("anvil/client: unexpected SUBSCRIBE ack: %q", line)
	}
	out := make(chan string, 16)
	// ctx-cancel closer: closing the conn forces ReadString to
	// return immediately, which lets the reader goroutine exit
	// and close `out`.
	go func() {
		<-ctx.Done()
		_ = c.conn.Close()
	}()
	go func() {
		defer close(out)
		for {
			line, err := c.rd.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
			if len(fields) < 3 || fields[0] != "EVENT" {
				continue
			}
			select {
			case out <- fields[2]:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// HeartbeatLoop is the recommended client-side renew pattern.
// It fires Heartbeat(id) every `interval` until ctx is cancelled
// or the underlying conn errors out. interval should be set to
// roughly TTL/3 so a single missed beat does not reap the
// session (DefaultSessionTTL / 3 ≈ 30s for the default config).
//
// On unknown-session response the loop logs a warning via the
// provided logger and exits — the caller is responsible for
// reconnecting + re-issuing CONNECT if it wants to keep the
// session registered.
//
// The returned error is non-nil only on a transport failure;
// graceful ctx cancellation returns nil.
func (c *Conn) HeartbeatLoop(ctx context.Context, id string, interval time.Duration, onUnknown func()) error {
	if interval <= 0 {
		return fmt.Errorf("anvil/client: heartbeat interval must be > 0")
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			ok, err := c.Heartbeat(id)
			if err != nil {
				return err
			}
			if !ok {
				if onUnknown != nil {
					onUnknown()
				}
				return nil
			}
		}
	}
}

// ErrTooManyConns is returned by Connect when the anvil server responds FAIL
// with reason=too-many-connections.
var ErrTooManyConns = fmt.Errorf("anvil: too many connections")
