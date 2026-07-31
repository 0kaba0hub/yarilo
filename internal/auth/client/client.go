// Package client implements the yarilo-auth TAB-delimited client protocol.
//
// A Client owns ONE persistent connection and multiplexes every request over
// it, which is what the wire protocol was always built for: each command
// carries a request id and the reply echoes it back. All public methods are
// safe for concurrent use from any number of goroutines — a login pod holds a
// single Client for its whole lifetime rather than dialling per login (#878).
//
// Why this matters: a fresh connection means a full mutual-TLS handshake, and
// measurement on sandbox showed 9469 connections for 9329 requests — one
// handshake per request, costing 1.73s per login against an 0.28s AUTH
// exchange. Reuse removes that cost from the hot path entirely.
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
	// ErrConnLost is returned when the connection dropped after the request
	// was already on the wire. Such a request is deliberately NOT retried: the
	// server may have processed it, and a second AUTH would double-count the
	// auth-penalty counter for that IP.
	ErrConnLost = errors.New("auth/client: connection lost mid-request")
	// ErrClosed is returned once Close has been called.
	ErrClosed = errors.New("auth/client: client closed")
	// ErrTimeout is returned when no reply arrived within the request timeout,
	// including time spent waiting for a reconnect to finish.
	ErrTimeout = errors.New("auth/client: request timed out")
)

// The three client timeouts below — dial, request, write — are deliberately
// hardcoded constants, NOT config knobs (#926). They are internal safety bounds
// of the protocol client, not behavioural parameters an operator has a
// legitimate reason to tune per deployment: a healthy write is microseconds, a
// dial is sub-second, and the request budget is already generous for the
// server's own tarpits. A value being hit means something is broken, not
// mistuned — exposing them would promise a tunability this class does not have,
// and yarilo's "every tunable is a config knob" rule is about Dovecot-modelled
// features and behavioural parameters, not socket-level guards (Dovecot itself
// keeps plenty of such constants). If a real need to tune them ever appears,
// expose all three together as one auth-client config section, never piecemeal.
const (
	defaultDialTimeout = 5 * time.Second
	// defaultRequestTimeout is generous on purpose: yarilo-auth deliberately
	// sleeps on the AUTH path (auth-penalty tarpit up to 15s, policy tarpit,
	// timing-leak failure delay), so a legitimate reply can take many seconds.
	// A tight timeout here would manufacture failures the server never had.
	defaultRequestTimeout = 30 * time.Second
	// defaultWriteTimeout bounds a single socket write (#926). A write to a
	// healthy peer drains into the send buffer in microseconds, so 5s already
	// means the connection is fundamentally broken — a blackholed peer (pod
	// killed without FIN/RST, conntrack dropped) whose send buffer has filled.
	// It is deliberately SEPARATE from RequestTimeout: an operator raising the
	// request budget for a slow passdb must not also stretch dead-socket
	// detection, which guards a different failure.
	defaultWriteTimeout = 5 * time.Second
	// keepAlive is the TCP keepalive idle period; keepAliveInterval/Count set the
	// probe cadence explicitly (#926) so a blackholed IDLE connection is torn
	// down in ~keepAlive + Count*Interval, not the ~11 min the Linux defaults
	// (75s * 9) produced in the incident.
	keepAlive         = 15 * time.Second
	keepAliveInterval = 15 * time.Second
	keepAliveCount    = 4
	// Reconnect backoff bounds (#932): the redial loop starts here and doubles up
	// to the cap, and NEVER gives up while the client is open. A caller waiting
	// on the reconnect still bails on its own request timeout, but the loop keeps
	// going, so the client recovers within maxReconnectBackoff of auth returning
	// — however long the outage — instead of becoming a permanent zombie.
	initialReconnectBackoff = 200 * time.Millisecond
	maxReconnectBackoff     = 5 * time.Second
)

// AuthResult carries the fields from a successful AUTH OK response.
type AuthResult struct {
	Username  string
	Nologin   bool
	AllowNets string
	Token     string
	// DirectorTag is the per-user director backend tag (#746), when the
	// passdb/userdb chain set one. Empty means no per-user override —
	// the login component's static director_tag config applies.
	DirectorTag string
}

// Options tunes a Client. Zero values select the documented defaults.
type Options struct {
	// RequestTimeout bounds one request end to end, including any wait for an
	// in-progress reconnect.
	RequestTimeout time.Duration
	// DialTimeout bounds a single connection attempt (TCP + TLS handshake).
	DialTimeout time.Duration
	// WriteTimeout bounds a single socket write so a blackholed peer cannot block
	// a write forever while holding the shared mutex (#926). 0 uses the default
	// (5s). Independent of RequestTimeout.
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

	// mu guards every field below. It is held across the socket write, which
	// is safe because requests are single short lines; it is never held while
	// waiting for a reply.
	mu      sync.Mutex
	conn    net.Conn
	state   connState
	gen     uint64        // bumped on every successful dial
	ready   chan struct{} // non-nil while reconnecting; closed on success
	pending map[string]chan string

	// done is closed by Close so the redial loop, which never gives up on its
	// own (#932), wakes from its backoff and exits promptly.
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
	id := c.nextID()

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
// A write failure is retried on a fresh connection: the error means the line —
// crucially including its terminating newline — did not make it out, and the
// server's reader only dispatches on a complete line, so the command cannot
// have been executed. A failure AFTER a successful write is not retried
// (ErrConnLost), because the server may have processed it and a repeated AUTH
// would double-count the auth-penalty counter.
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
		// Bound the write so a blackholed peer whose send buffer has filled
		// cannot block here forever while holding c.mu, wedging every other
		// caller parked on c.mu.Lock() (#926). On timeout the write returns an
		// error and we fall through to beginReconnect below.
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

// awaitLive blocks while a reconnect is in progress so a caller queues instead
// of failing. This is the whole point of the reconnect design: during a
// yarilo-auth rollout the logins wait a few hundred milliseconds and succeed,
// rather than each being told the service is unavailable.
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
			// Invariant (#932): redial never abandons an open client, so `ready`
			// is non-nil for as long as the state is reconnecting. A nil here can
			// therefore only be a benign lost race with a redial that just went
			// live between the switch above and this read — re-loop so the switch
			// observes stateLive, rather than mis-reporting "ready" (the old
			// behaviour that turned the give-up state into a busy-spin zombie).
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

// beginReconnect transitions to reconnecting and redials, once per generation.
// Concurrent callers that observed the same broken generation all wait on the
// single redial rather than each opening their own connection.
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
	// Requests already on the wire cannot be safely replayed — hand them
	// ErrConnLost instead of silently retrying.
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
	// The redial goroutine OWNS recovery and never gives up while the client is
	// open (#932). An auth outage that outlasts a fixed retry budget (a pod
	// replacement is 10-60s) previously made this return, leaving the client
	// stuck in stateReconnecting with no goroutine able to revive it — a
	// permanent, CPU-burning zombie, since no request path calls beginReconnect
	// again. Instead we loop with a capped exponential backoff until a dial
	// succeeds or Close wakes us.
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
			// Malformed reply carries no id, so it cannot be routed. Dropping
			// it is correct: the waiter times out rather than being handed a
			// reply that may belong to someone else.
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
	// Explicit keepalive cadence (#926): net.Dialer.KeepAlive alone sets only the
	// idle period and leaves interval/count at the OS defaults (Linux 75s * 9 ≈
	// 11 min), which is what let a blackholed idle connection sit undetected in
	// the incident. Idle + Count*Interval bounds detection to ~75s.
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
// handshake banner (which carries no request id) is never seen by the
// demultiplexer.
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
