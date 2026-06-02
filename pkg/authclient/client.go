package authclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
)

// defaultDialTimeout caps the initial TCP / TLS handshake so a
// reachable-but-stalled master listener cannot wedge startup
// indefinitely. Callers that need different bounds pass a tweaked
// dialer via [DialContext].
const defaultDialTimeout = 5 * time.Second

// ErrNotImplemented is returned by [Client.PassdbLookup] until
// Phase AUTH-2 ships Passdb.LookupCredentials and the master-protocol
// PASS handler stops returning the corresponding `FAIL reason=PASS
// not implemented` line. Callers can use errors.Is to gate
// fall-through behaviour cleanly.
var ErrNotImplemented = errors.New("authclient: PASS not implemented (Phase AUTH-2)")

// ErrClosed signals that a Client has had Close called or its
// connection terminated; subsequent calls return this so callers
// can re-Dial deterministically rather than hanging on the dead
// reader.
var ErrClosed = errors.New("authclient: client closed")

// Client speaks the yarilo-auth master protocol over a single TCP
// (optionally mTLS-wrapped) connection. Methods are safe to call
// from multiple goroutines: a mutex enforces one in-flight request
// at a time on the underlying conn, mirroring the server-side
// per-connection serial-processing guarantee.
//
// Construct with [Dial] / [DialContext]. Always call [Client.Close]
// on shutdown so the FIN propagates promptly to the server side and
// the goroutine watching that conn there returns.
type Client struct {
	conn   net.Conn
	rd     *bufio.Reader
	mu     sync.Mutex
	nextID atomic.Uint64
	closed atomic.Bool
}

// Dial opens a connection to the yarilo-auth master listener at
// addr and consumes the protocol handshake. When tlsCfg is non-nil
// the connection is wrapped in TLS — yarilo's standard deployment
// supplies an mTLS config built from the internal CA + the
// consumer's client cert.
//
// The handshake's VERSION line is validated: only major version 1
// is accepted today. Future major bumps surface as a typed error so
// callers can fail fast rather than speak stale dialect.
func Dial(addr string, tlsCfg *tls.Config) (*Client, error) {
	return DialContext(context.Background(), addr, tlsCfg)
}

// DialContext is Dial with a context-aware dial. The context bounds
// the TCP + TLS handshake; once the handshake is consumed the
// context has no further effect on the returned Client.
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
	c := &Client{conn: conn, rd: bufio.NewReader(conn)}
	if err := c.consumeHandshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// consumeHandshake drains the server-side handshake (VERSION, SPID,
// CUID, COOKIE, DONE) and validates the version line. Subsequent
// reads on c.rd see only command responses.
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
			// informational — server identity / cookie. Recorded
			// only for telemetry; the cookie is not used to
			// authenticate subsequent commands on this client.
		}
	}
	if !verSeen {
		return errors.New("authclient: handshake missing VERSION line")
	}
	return nil
}

// Close releases the underlying connection. Idempotent — calling
// Close twice returns nil the second time. After Close every other
// method on this Client returns [ErrClosed].
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
// Mirrors the [protocol.Userdb] contract so callers chaining
// Client.Userdb with other Userdb implementations see the same
// "not found is nil/nil" semantics.
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

// PassdbLookup runs a PASS lookup. The master-protocol handler for
// PASS returns `FAIL reason=PASS not implemented` until Phase AUTH-2
// ships Passdb.LookupCredentials; this method therefore always
// returns [ErrNotImplemented]. The full method exists in PR 3 so
// consumers can ship the typed call surface unchanged when AUTH-2
// flips the implementation on.
func (c *Client) PassdbLookup(ctx context.Context, username string) (*protocol.UserInfo, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	id := c.allocID()
	line, err := c.exchange(ctx, fmt.Sprintf("PASS\t%s\t%s\n", id, username))
	if err != nil {
		return nil, err
	}
	// The expected server reply is `FAIL\t<id>\treason=PASS not
	// implemented...`. Distinguish that from a real backend error
	// so consumers can errors.Is(err, ErrNotImplemented) cleanly.
	if reason, ok := extractFailReason(line, id); ok {
		if strings.HasPrefix(reason, "PASS not implemented") {
			return nil, ErrNotImplemented
		}
		return nil, fmt.Errorf("authclient: PASS %s: %s", username, reason)
	}
	// Phase AUTH-2 will start returning PASS hits — parse like USER.
	return c.parseUserResponse(line, id, username)
}

// IterateUsers runs a LIST and collects every streamed username
// until the server emits the DONE marker. Backends that do not
// support enumeration return a FAIL line; this method surfaces
// that as a typed error (compare its message against the wire
// reason if needed).
func (c *Client) IterateUsers(ctx context.Context) ([]string, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	id := c.allocID()
	c.mu.Lock()
	defer c.mu.Unlock()
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

// CacheFlush invokes the master-protocol CACHE-FLUSH verb. Empty
// masks slice triggers a full flush; otherwise each mask is sent
// as an additional tab-separated argument and yarilo-auth evicts
// every entry whose stored username matches any mask (glob
// syntax: `*` = any run, `?` = one char).
//
// Returns the count of evicted entries reported by the server.
// Used by `yarilo-admin auth cache flush [mask…]`.
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

// exchange writes a single-line request under c.mu and reads the
// matching single-line response. Used by USER and PASS — LIST has
// its own multi-line loop above.
func (c *Client) exchange(ctx context.Context, line string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

// parseUserResponse handles USER hit / NOTFOUND / FAIL responses
// keyed by id. The username argument is the value the caller
// supplied — used to set UserInfo.Username when the wire-side
// username token matches (defensive against a server bug, the
// caller's value wins).
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

// extractFailReason returns (reason, true) when line is a FAIL with
// the expected id. The OK==false return path lets callers
// distinguish "FAIL for me" from "this is some other frame" without
// a second parse pass.
func extractFailReason(line, id string) (string, bool) {
	parts := strings.Split(line, "\t")
	if len(parts) < 2 || parts[0] != "FAIL" || parts[1] != id {
		return "", false
	}
	return failReason(parts), true
}

// failReason concatenates the `key=value` tokens after the FAIL
// header into a human-readable reason. Falls back to the joined
// tokens when no `reason=` token is present so the caller still
// gets diagnostic context.
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

// applyDeadline ports the request's ctx Deadline onto the
// underlying net.Conn so a slow server triggers a read / write
// deadline rather than hanging the caller's goroutine indefinitely.
// Returns ctx.Err immediately when ctx is already done.
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
