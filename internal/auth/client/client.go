// Package client implements the yarilo-auth TAB-delimited client protocol.
//
// A Client owns one persistent connection and multiplexes all requests over
// it: each command carries a request id, the reply echoes it back. Safe for
// concurrent use; hold one Client per process instead of dialing per login,
// which would add a full mutual-TLS handshake to every request.
package client

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by Authenticate, Verify, and LookupUser.
var (
	ErrAuthFailed   = errors.New("auth/client: authentication failed")
	ErrTempFail     = errors.New("auth/client: temporary backend failure")
	ErrUserNotFound = errors.New("auth/client: user not found")
	// ErrConnLost is returned when the connection dropped after the request was
	// written. Not retried: the server may have processed it, and a second AUTH
	// would double-count the auth-penalty counter.
	ErrConnLost = errors.New("auth/client: connection lost mid-request")
	// ErrClosed is returned once Close has been called.
	ErrClosed = errors.New("auth/client: client closed")
	// ErrTimeout is returned when no reply arrived within the request timeout,
	// including time spent waiting for a reconnect to finish.
	ErrTimeout = errors.New("auth/client: request timed out")
)

// Timeouts are internal safety bounds, not config knobs: hitting one means the
// connection is broken, not mistuned (#926).
const (
	defaultDialTimeout = 5 * time.Second
	// Generous on purpose: the server sleeps on the AUTH path (auth-penalty
	// tarpit up to 15s, failure delay), so a legitimate reply can take seconds.
	defaultRequestTimeout = 30 * time.Second
	// Bounds a single socket write. Separate from RequestTimeout: a healthy
	// write drains into the send buffer in microseconds, so 5s means a
	// blackholed peer, whatever the request budget is.
	defaultWriteTimeout = 5 * time.Second
	// Explicit probe cadence tears down a blackholed idle connection in
	// ~keepAlive + Count*Interval instead of the ~11 min Linux defaults.
	keepAlive         = 15 * time.Second
	keepAliveInterval = 15 * time.Second
	keepAliveCount    = 4
	// Redial backoff doubles up to the cap and never gives up while the client
	// is open; callers still bail on their own request timeout.
	initialReconnectBackoff = 200 * time.Millisecond
	maxReconnectBackoff     = 5 * time.Second
)

// AuthResult carries the fields from a successful AUTH OK response.
type AuthResult struct {
	Username  string
	Nologin   bool
	AllowNets string
	Token     string
	// DirectorTag is the per-user director backend tag, if the passdb/userdb
	// chain set one. Empty means the static director_tag config applies.
	DirectorTag string
}

// Options tunes a Client. Zero values select the documented defaults.
type Options struct {
	// RequestTimeout bounds one request end to end, including any wait for an
	// in-progress reconnect.
	RequestTimeout time.Duration
	// DialTimeout bounds a single connection attempt (TCP + TLS handshake).
	DialTimeout time.Duration
	// WriteTimeout bounds a single socket write so a blackholed peer cannot
	// block forever while holding the shared mutex. Independent of
	// RequestTimeout.
	WriteTimeout time.Duration
}

type connState int

const (
	stateLive connState = iota
	stateReconnecting
	stateClosed
)

// Client is a persistent, multiplexed connection to yarilo-auth.
type Client struct {
	addr   string
	tlsCfg *tls.Config
	opts   Options

	seq atomic.Uint64

	// mu guards the fields below. Held across the socket write (requests are
	// single short lines), never while waiting for a reply.
	mu      sync.Mutex
	conn    net.Conn
	state   connState
	gen     uint64        // bumped on every successful dial
	ready   chan struct{} // non-nil while reconnecting; closed on success
	pending map[string]chan string

	// done is closed by Close so the redial loop wakes from its backoff and
	// exits.
	done chan struct{}
}

// Dial connects to addr (TCP or Unix socket) and performs the VERSION
// handshake. tlsCfg may be nil for plain (non-TLS) connections.
func Dial(addr string, tlsCfg *tls.Config) (*Client, error) {
	return New(addr, tlsCfg, Options{})
}

// New is Dial with explicit Options.
func New(addr string, tlsCfg *tls.Config, opts Options) (*Client, error) {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = defaultDialTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}
	c := &Client{
		addr:    addr,
		tlsCfg:  tlsCfg,
		opts:    opts,
		pending: make(map[string]chan string),
		done:    make(chan struct{}),
	}
	conn, rd, err := c.dial()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.conn = conn
	c.state = stateLive
	c.gen++
	gen := c.gen
	c.mu.Unlock()
	go c.readLoop(rd, gen)
	return c, nil
}

// Close closes the connection and fails every in-flight request.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.state == stateClosed {
		c.mu.Unlock()
		return nil
	}
	c.state = stateClosed
	close(c.done)
	conn := c.conn
	c.conn = nil
	pending := c.pending
	c.pending = make(map[string]chan string)
	if c.ready != nil {
		close(c.ready)
		c.ready = nil
	}
	c.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Authenticate sends an AUTH command for the given credentials and returns the
// result. remoteIP and sessionID may be empty strings (omitted from request).
func (c *Client) Authenticate(username, password, service, remoteIP, sessionID string) (*AuthResult, error) {
	return c.AuthenticateAs("", username, password, service, remoteIP, sessionID)
}

// AuthenticateAs is Authenticate with an impersonation target: authzid is the
// user the caller asks to act AS, authcid the identity it proves. Empty authzid
// is an ordinary login, which is what Authenticate passes.
//
// The target travels in the SASL PLAIN response, where the auth service already
// reads it. Rebuilding the response with an empty authzid -- as this client did
// -- discarded every master login before it left the pod, so the service saw a
// plain login of the master and refused it (#1305). Whether impersonation is
// granted stays the service's decision; the client only carries the request.
func (c *Client) AuthenticateAs(authzid, authcid, password, service, remoteIP, sessionID string) (*AuthResult, error) {
	id := c.nextID()

	var sb strings.Builder
	sb.WriteString("AUTH\t")
	sb.WriteString(id)
	sb.WriteString("\tPLAIN")
	sb.WriteString("\tuser=")
	sb.WriteString(authcid)
	sb.WriteString("\tresp=")
	// SASL PLAIN: [authzid] NUL authcid NUL password
	sb.WriteString(authzid)
	sb.WriteString("\x00")
	sb.WriteString(authcid)
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

	line, err := c.exchange(id, sb.String())
	if err != nil {
		return nil, err
	}
	return parseAuthResponse(line)
}

// LookupUser sends a USER command to yarilo-auth and returns whether the user
// exists in the userdb. Returns ErrUserNotFound when the user is unknown,
// ErrTempFail on a transient backend error.
func (c *Client) LookupUser(username string) (bool, error) {
	id := c.nextID()
	line, err := c.exchange(id, fmt.Sprintf("USER\t%s\t%s", id, username))
	if err != nil {
		return false, err
	}
	fields := strings.Split(line, "\t")
	switch fields[0] {
	case "USER":
		return true, nil
	case "NOTFOUND":
		return false, ErrUserNotFound
	case "FAIL":
		return false, ErrTempFail
	default:
		return false, fmt.Errorf("auth/client: unknown USER response verb %q", fields[0])
	}
}

// Verify sends a VERIFY command and returns the username, session ID, and
// service bound to the token. username and sessionID are sent as binding
// claims — the server rejects the token if they don't match what was stored
// at issue time. Returns ErrAuthFailed when the token is unknown, expired,
// or the claims don't match.
func (c *Client) Verify(token, username, sessionID string) (string, string, string, error) {
	id := c.nextID()
	req := fmt.Sprintf("VERIFY\t%s\t%s\tuser=%s\tsession=%s", id, token, username, sessionID)
	line, err := c.exchange(id, req)
	if err != nil {
		return "", "", "", err
	}
	return parseVerifyResponse(line)
}

func (c *Client) nextID() string {
	return strconv.FormatUint(c.seq.Add(1), 10)
}

// exchange registers id, writes req, and waits for the matching reply.
//
// A write failure is retried on a fresh connection: the server dispatches only
// on a complete line, so a partial write cannot have been executed. A failure
// after a successful write returns ErrConnLost instead — the server may have
// processed it, and a repeated AUTH would double-count the auth penalty.
func (c *Client) exchange(id, req string) (string, error) {
	deadline := time.Now().Add(c.opts.RequestTimeout)
	ch := make(chan string, 1)

	for {
		if err := c.awaitLive(deadline); err != nil {
			return "", err
		}

		c.mu.Lock()
		if c.state == stateClosed {
			c.mu.Unlock()
			return "", ErrClosed
		}
		if c.state != stateLive {
			c.mu.Unlock()
			continue // lost the race with a reconnect; wait again
		}
		c.pending[id] = ch
		gen := c.gen
		// Bound the write so a blackholed peer cannot block forever while
		// holding c.mu; on timeout we fall through to beginReconnect.
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
		_, werr := fmt.Fprintln(c.conn, req)
		c.mu.Unlock()

		if werr == nil {
			break
		}

		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.beginReconnect(gen)
		if time.Now().After(deadline) {
			return "", ErrTimeout
		}
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case line, ok := <-ch:
		if !ok {
			return "", ErrConnLost
		}
		return line, nil
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return "", ErrTimeout
	}
}

// awaitLive blocks while a reconnect is in progress so callers queue instead
// of failing during a short auth outage.
func (c *Client) awaitLive(deadline time.Time) error {
	for {
		c.mu.Lock()
		switch c.state {
		case stateClosed:
			c.mu.Unlock()
			return ErrClosed
		case stateLive:
			c.mu.Unlock()
			return nil
		}
		ready := c.ready
		c.mu.Unlock()

		if ready == nil {
			// ready is non-nil for as long as state is reconnecting; nil here
			// is only a lost race with a redial that just went live. Re-loop so
			// the switch observes stateLive.
			continue
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ready:
			timer.Stop()
		case <-timer.C:
			return ErrTimeout
		}
	}
}

// beginReconnect transitions to reconnecting and redials, once per generation;
// concurrent callers wait on the single redial.
func (c *Client) beginReconnect(gen uint64) {
	c.mu.Lock()
	if c.state == stateClosed || c.gen != gen || c.state == stateReconnecting {
		c.mu.Unlock()
		return
	}
	c.state = stateReconnecting
	c.ready = make(chan struct{})
	old := c.conn
	c.conn = nil
	// Requests already on the wire cannot be safely replayed; fail them with
	// ErrConnLost.
	inflight := c.pending
	c.pending = make(map[string]chan string)
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	for _, ch := range inflight {
		close(ch)
	}

	go c.redial()
}

func (c *Client) redial() {
	// The redial goroutine owns recovery: loop with capped backoff until a dial
	// succeeds or Close wakes us. Giving up would leave the client stuck in
	// stateReconnecting with nothing to revive it (#932).
	backoff := initialReconnectBackoff
	for {
		conn, rd, derr := c.dial()
		if derr == nil {
			c.mu.Lock()
			if c.state == stateClosed {
				c.mu.Unlock()
				_ = conn.Close()
				return
			}
			ready := c.ready
			c.ready = nil
			c.conn = conn
			c.state = stateLive
			c.gen++
			gen := c.gen
			c.mu.Unlock()
			if ready != nil {
				close(ready)
			}
			go c.readLoop(rd, gen)
			return
		}

		c.mu.Lock()
		closed := c.state == stateClosed
		c.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-time.After(backoff):
		case <-c.done:
			return
		}
		if backoff *= 2; backoff > maxReconnectBackoff {
			backoff = maxReconnectBackoff
		}
	}
}

// readLoop demultiplexes replies by request id until the connection breaks.
func (c *Client) readLoop(rd *bufio.Reader, gen uint64) {
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			c.beginReconnect(gen)
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			// No id, cannot be routed; drop it and let the waiter time out.
			continue
		}
		id := fields[1]
		c.mu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if ok {
			ch <- line
		}
	}
}

func (c *Client) dial() (net.Conn, *bufio.Reader, error) {
	// net.Dialer.KeepAlive alone leaves interval/count at the OS defaults
	// (Linux 75s * 9 ≈ 11 min); explicit cadence bounds blackhole detection to
	// ~75s.
	dialer := &net.Dialer{
		Timeout: c.opts.DialTimeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     keepAlive,
			Interval: keepAliveInterval,
			Count:    keepAliveCount,
		},
	}
	var raw net.Conn
	var err error
	if c.tlsCfg != nil {
		raw, err = tls.DialWithDialer(dialer, "tcp", c.addr, c.tlsCfg)
	} else {
		raw, err = dialer.Dial("tcp", c.addr)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("auth/client: dial %s: %w", c.addr, err)
	}
	rd := bufio.NewReader(raw)
	if err := handshake(raw, rd); err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	return raw, rd, nil
}

// handshake exchanges VERSION lines. Runs before readLoop starts, so the
// id-less banner is never seen by the demultiplexer.
func handshake(conn net.Conn, rd *bufio.Reader) error {
	if _, err := fmt.Fprintln(conn, "VERSION\t1\t0"); err != nil {
		return fmt.Errorf("auth/client: handshake write: %w", err)
	}
	gotVersion := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return fmt.Errorf("auth/client: handshake read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
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

func parseAuthResponse(line string) (*AuthResult, error) {
	fields := strings.Split(line, "\t")
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
			case strings.HasPrefix(f, "director_tag="):
				res.DirectorTag = f[len("director_tag="):]
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

func parseVerifyResponse(line string) (string, string, string, error) {
	fields := strings.Split(line, "\t")
	switch fields[0] {
	case "OK":
		var username, sessionID, service string
		for _, f := range fields[2:] {
			switch {
			case strings.HasPrefix(f, "user="):
				username = f[len("user="):]
			case strings.HasPrefix(f, "session="):
				sessionID = f[len("session="):]
			case strings.HasPrefix(f, "service="):
				service = f[len("service="):]
			}
		}
		return username, sessionID, service, nil
	case "FAIL":
		return "", "", "", ErrAuthFailed
	default:
		return "", "", "", fmt.Errorf("auth/client: unknown verify verb %q", fields[0])
	}
}
