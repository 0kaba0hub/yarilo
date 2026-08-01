package authclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// defaultDialTimeout caps the initial TCP/TLS handshake so a stalled master
// listener cannot wedge startup. Use [DialContext] for a different bound.
const defaultDialTimeout = 5 * time.Second

// ErrNotImplemented is returned by [Client.PassdbLookup] while the master
// PASS handler still replies `FAIL reason=PASS not implemented`. Callers can
// errors.Is against it to gate fall-through behaviour.
var ErrNotImplemented = errors.New("authclient: PASS not implemented (Phase AUTH-2)")

// ErrClosed signals that a Client was Closed or its connection terminated.
// Subsequent calls return it so callers can re-Dial rather than hang.
var ErrClosed = errors.New("authclient: client closed")

// Client speaks the yarilo-auth master protocol over a single TCP (optionally
// mTLS-wrapped) connection. Methods are safe for concurrent use: a mutex
// enforces one in-flight request at a time on the underlying conn.
//
// When the connection breaks (e.g. yarilo-auth restart) the next call redials
// and retries once. [Client.Close] disables reconnect permanently.
//
// Construct with [Dial] / [DialContext]. Always [Client.Close] on shutdown so
// the FIN reaches the server promptly.
type Client struct {
	conn   net.Conn
	rd     *bufio.Reader
	mu     sync.Mutex
	nextID atomic.Uint64
	closed atomic.Bool

	addr   string
	tlsCfg *tls.Config
}

// Dial opens a connection to the yarilo-auth master listener at addr and
// consumes the handshake. A non-nil tlsCfg wraps the connection in (m)TLS.
// Only major version 1 is accepted; other versions return a typed error.
func Dial(addr string, tlsCfg *tls.Config) (*Client, error) {
	return DialContext(context.Background(), addr, tlsCfg)
}

// DialContext is Dial with a context that bounds the TCP + TLS handshake.
// After the handshake the context no longer affects the returned Client.
func DialContext(ctx context.Context, addr string, tlsCfg *tls.Config) (*Client, error) {
	d := net.Dialer{Timeout: defaultDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("authclient: dial %s: %w", addr, err)
	}
	if tlsCfg != nil {
		tc := tls.Client(conn, tlsCfg)
		if dl, ok := ctx.Deadline(); ok {
			_ = tc.SetDeadline(dl)
		}
		if err := tc.HandshakeContext(ctx); err != nil {
			tc.Close()
			return nil, fmt.Errorf("authclient: tls handshake %s: %w", addr, err)
		}
		_ = tc.SetDeadline(time.Time{})
		conn = tc
	}
	c := &Client{conn: conn, rd: bufio.NewReader(conn), addr: addr, tlsCfg: tlsCfg}
	if err := c.consumeHandshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// consumeHandshake drains the server handshake (VERSION, SPID, CUID, COOKIE,
// DONE) and validates the version line. Later reads see only command responses.
func (c *Client) consumeHandshake() error {
	verSeen := false
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			return fmt.Errorf("authclient: read handshake: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "DONE" {
			break
		}
		parts := strings.Split(line, "\t")
		switch parts[0] {
		case "VERSION":
			if len(parts) != 4 {
				return fmt.Errorf("authclient: malformed VERSION %q", line)
			}
			if parts[1] != "yarilo-auth-master" {
				return fmt.Errorf("authclient: unexpected protocol %q (want yarilo-auth-master)", parts[1])
			}
			if parts[2] != "1" {
				return fmt.Errorf("authclient: unsupported major version %q (want 1)", parts[2])
			}
			verSeen = true
		case "SPID", "CUID", "COOKIE":
			// informational only; the cookie does not authenticate
			// subsequent commands on this client.
		}
	}
	if !verSeen {
		return errors.New("authclient: handshake missing VERSION line")
	}
	return nil
}

// Close releases the underlying connection. Idempotent. After Close every
// other method returns [ErrClosed].
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.conn.Close()
}

// Userdb runs a USER lookup for username. Returns:
//
//   - (ui, nil)  — backend hit; ui populated from the wire fields
//   - (nil, nil) — backend miss (master responded NOTFOUND)
//   - (nil, err) — wire / parse / backend error
//
// Mirrors the [protocol.Userdb] "not found is nil/nil" contract.
func (c *Client) Userdb(ctx context.Context, username string) (*protocol.UserInfo, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	id := c.allocID()
	line, err := c.exchange(ctx, fmt.Sprintf("USER\t%s\t%s\n", id, username))
	if err != nil {
		return nil, err
	}
	return c.parseUserResponse(line, id, username)
}

// PassdbLookup runs a PASS lookup. The master PASS handler still replies
// `FAIL reason=PASS not implemented`, so this method always returns
// [ErrNotImplemented]; the typed call surface is stable for when it lands.
func (c *Client) PassdbLookup(ctx context.Context, username string) (*protocol.UserInfo, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	id := c.allocID()
	line, err := c.exchange(ctx, fmt.Sprintf("PASS\t%s\t%s\n", id, username))
	if err != nil {
		return nil, err
	}
	// Distinguish the `FAIL reason=PASS not implemented` reply from a real
	// backend error so callers can errors.Is(err, ErrNotImplemented).
	if reason, ok := extractFailReason(line, id); ok {
		if strings.HasPrefix(reason, "PASS not implemented") {
			return nil, ErrNotImplemented
		}
		return nil, fmt.Errorf("authclient: PASS %s: %s", username, reason)
	}
	// A PASS hit parses like USER.
	return c.parseUserResponse(line, id, username)
}

// IterateUsers runs a LIST and collects streamed usernames until the DONE
// marker. Backends without enumeration reply FAIL, surfaced as an error.
func (c *Client) IterateUsers(ctx context.Context) ([]string, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := c.iterateUsersLocked(ctx)
	if err != nil && !c.closed.Load() && isConnectionError(err) {
		if rerr := c.redial(); rerr == nil {
			out, err = c.iterateUsersLocked(ctx)
		}
	}
	return out, err
}

func (c *Client) iterateUsersLocked(ctx context.Context) ([]string, error) {
	id := c.allocID()
	if err := c.applyDeadline(ctx); err != nil {
		return nil, err
	}
	defer c.clearDeadline()
	if _, err := c.conn.Write([]byte(fmt.Sprintf("LIST\t%s\n", id))); err != nil {
		return nil, fmt.Errorf("authclient: write LIST: %w", err)
	}
	var out []string
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("authclient: read LIST: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		parts := strings.Split(line, "\t")
		if len(parts) < 2 || parts[1] != id {
			return nil, fmt.Errorf("authclient: LIST response for unexpected id: %q", line)
		}
		switch parts[0] {
		case "LIST":
			if len(parts) < 3 {
				return nil, fmt.Errorf("authclient: malformed LIST entry: %q", line)
			}
			out = append(out, parts[2])
		case "DONE":
			return out, nil
		case "FAIL":
			return nil, fmt.Errorf("authclient: LIST: %s", failReason(parts))
		default:
			return nil, fmt.Errorf("authclient: unexpected LIST frame: %q", line)
		}
	}
}

// CacheFlush invokes the CACHE-FLUSH verb. An empty masks slice flushes
// everything; otherwise each mask is sent as a tab-separated argument and
// entries whose username matches any mask are evicted (glob: `*`, `?`).
// Returns the evicted-entry count reported by the server.
func (c *Client) CacheFlush(ctx context.Context, masks []string) (uint32, error) {
	if c.closed.Load() {
		return 0, ErrClosed
	}
	id := c.allocID()
	cmd := "CACHE-FLUSH\t" + id
	for _, m := range masks {
		cmd += "\t" + m
	}
	cmd += "\n"
	resp, err := c.exchange(ctx, cmd)
	if err != nil {
		return 0, err
	}
	parts := strings.Split(resp, "\t")
	if len(parts) < 2 || parts[1] != id {
		return 0, fmt.Errorf("authclient: CACHE-FLUSH response for unexpected id: %q", resp)
	}
	switch parts[0] {
	case "OK":
		if len(parts) < 3 {
			return 0, fmt.Errorf("authclient: CACHE-FLUSH OK without count: %q", resp)
		}
		n, perr := strconv.ParseUint(parts[2], 10, 32)
		if perr != nil {
			return 0, fmt.Errorf("authclient: CACHE-FLUSH count parse: %w", perr)
		}
		return uint32(n), nil
	case "FAIL":
		return 0, fmt.Errorf("authclient: CACHE-FLUSH: %s", failReason(parts))
	default:
		return 0, fmt.Errorf("authclient: unexpected CACHE-FLUSH frame: %q", resp)
	}
}

// IssueSession sends a SESSION command and returns a one-time token bound to
// username. The token is scoped to the "lmtp" service and must be consumed by
// a VERIFY on the client listener within its TTL.
//
// sid is the warden session ID for this delivery; ip is the originating MTA
// address. Both are audit-logged only and do not affect token validity.
func (c *Client) IssueSession(ctx context.Context, username, sid, ip string) (string, error) {
	if c.closed.Load() {
		return "", ErrClosed
	}
	id := c.allocID()
	cmd := fmt.Sprintf("SESSION\t%s\tuser=%s\tsid=%s\tip=%s\n", id, username, sid, ip)
	resp, err := c.exchange(ctx, cmd)
	if err != nil {
		return "", err
	}
	parts := strings.Split(resp, "\t")
	if len(parts) < 2 || parts[1] != id {
		return "", fmt.Errorf("authclient: SESSION response for unexpected id: %q", resp)
	}
	switch parts[0] {
	case "OK":
		for _, p := range parts[2:] {
			if strings.HasPrefix(p, "token=") {
				return strings.TrimPrefix(p, "token="), nil
			}
		}
		return "", fmt.Errorf("authclient: SESSION OK missing token field: %q", resp)
	case "FAIL":
		return "", fmt.Errorf("authclient: SESSION %s: %s", username, failReason(parts))
	default:
		return "", fmt.Errorf("authclient: unexpected SESSION frame: %q", resp)
	}
}

// exchange writes a single-line request under c.mu and reads the matching
// single-line response, redialing once on a connection error. LIST has its
// own multi-line loop.
func (c *Client) exchange(ctx context.Context, line string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := c.exchangeLocked(ctx, line)
	if err != nil && !c.closed.Load() && isConnectionError(err) {
		if rerr := c.redial(); rerr == nil {
			resp, err = c.exchangeLocked(ctx, line)
		}
	}
	return resp, err
}

// exchangeLocked performs one write+read pair; c.mu must be held.
func (c *Client) exchangeLocked(ctx context.Context, line string) (string, error) {
	if err := c.applyDeadline(ctx); err != nil {
		return "", err
	}
	defer c.clearDeadline()
	if _, err := c.conn.Write([]byte(line)); err != nil {
		return "", fmt.Errorf("authclient: write: %w", err)
	}
	resp, err := c.rd.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("authclient: read: %w", err)
	}
	return strings.TrimRight(resp, "\r\n"), nil
}

// redial closes the dead connection and opens a fresh one, replacing c.conn
// and c.rd on success. Must be called with c.mu held.
func (c *Client) redial() error {
	_ = c.conn.Close()
	d := net.Dialer{Timeout: defaultDialTimeout}
	conn, err := d.DialContext(context.Background(), "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("authclient: redial %s: %w", c.addr, err)
	}
	if c.tlsCfg != nil {
		tc := tls.Client(conn, c.tlsCfg)
		if err := tc.HandshakeContext(context.Background()); err != nil {
			tc.Close()
			return fmt.Errorf("authclient: redial tls %s: %w", c.addr, err)
		}
		conn = tc
	}
	c.conn = conn
	c.rd = bufio.NewReader(conn)
	if err := c.consumeHandshake(); err != nil {
		conn.Close()
		return err
	}
	return nil
}

// isConnectionError reports whether err is a broken connection that warrants
// a reconnect. Context cancellations and timeouts are excluded — they are
// caller/server issues, not a dead socket.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return !opErr.Timeout()
	}
	return false
}

// parseUserResponse handles USER hit / NOTFOUND / FAIL responses keyed by id.
// The caller-supplied username fills UserInfo.Username when the wire token is
// empty, so the caller's value wins.
func (c *Client) parseUserResponse(line, id, username string) (*protocol.UserInfo, error) {
	parts := strings.Split(line, "\t")
	if len(parts) < 2 || parts[1] != id {
		return nil, fmt.Errorf("authclient: response for unexpected id: %q", line)
	}
	switch parts[0] {
	case "USER":
		if len(parts) < 3 {
			return nil, fmt.Errorf("authclient: malformed USER response: %q", line)
		}
		info, err := protocol.ParseUserInfo(parts[3:])
		if err != nil {
			return nil, fmt.Errorf("authclient: USER fields: %w", err)
		}
		info.Username = parts[2]
		if info.Username == "" {
			info.Username = username
		}
		return info, nil
	case "NOTFOUND":
		return nil, nil
	case "FAIL":
		return nil, fmt.Errorf("authclient: USER %s: %s", username, failReason(parts))
	default:
		return nil, fmt.Errorf("authclient: unexpected USER frame: %q", line)
	}
}

// extractFailReason returns (reason, true) when line is a FAIL with the
// expected id, letting callers tell "FAIL for me" from another frame.
func extractFailReason(line, id string) (string, bool) {
	parts := strings.Split(line, "\t")
	if len(parts) < 2 || parts[0] != "FAIL" || parts[1] != id {
		return "", false
	}
	return failReason(parts), true
}

// failReason returns the `reason=` token after the FAIL header, falling back
// to the joined tokens when none is present.
func failReason(parts []string) string {
	for _, p := range parts[2:] {
		if strings.HasPrefix(p, "reason=") {
			return strings.TrimPrefix(p, "reason=")
		}
	}
	if len(parts) > 2 {
		return strings.Join(parts[2:], " ")
	}
	return "unknown failure"
}

func (c *Client) allocID() string {
	return strconv.FormatUint(c.nextID.Add(1), 10)
}

// applyDeadline ports the ctx deadline onto the net.Conn so a slow server hits
// a read/write deadline instead of hanging. Returns ctx.Err if ctx is done.
func (c *Client) applyDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		return c.conn.SetDeadline(dl)
	}
	return nil
}

func (c *Client) clearDeadline() {
	_ = c.conn.SetDeadline(time.Time{})
}
