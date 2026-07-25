// Package backendreg is the backend side of the director backend-liveness
// model (#776): a backend session process self-registers with the director
// and heartbeats, so the director's hash ring reflects live backend pods
// without a one-time DNS resolve or an external prober. See
// docs/DEPLOYMENT.md "backend liveness — self-registration + heartbeat".
package backendreg

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	reconnectBase = 1 * time.Second
	reconnectMax  = 30 * time.Second
	dialTimeout   = 10 * time.Second
)

// Options configures a Client.
type Options struct {
	// DirectorAddr is the director's ClusterIP Service "host:port". It does
	// not matter which director replica answers — the registration is
	// gossiped ring-wide, so a load-balanced address is correct.
	DirectorAddr string
	// SelfIP / Port / Tag identify THIS backend in the ring; Port is the
	// session port the login proxies dial. Vhosts is the ring weight (0 =
	// director default 100).
	SelfIP string
	Port   int
	Tag    string
	Vhosts int
	// Interval paces the heartbeat (BACKEND-UP re-register). 0 = 10s.
	Interval time.Duration
	// Healthy gates the heartbeat on self-health: BACKEND-UP is sent ONLY
	// while this returns true (the /readyz condition), so a wedged data
	// path (accept stalled, sessions blocked on I/O) stops heartbeating and
	// is expired ring-wide rather than kept as a live-but-dead target.
	Healthy func() bool
	// TLS wraps the director connection when non-nil (internal mTLS).
	TLS *tls.Config
}

// Client maintains a registration connection to the director and sends a
// seq'd BACKEND-UP heartbeat while healthy, a BACKEND-DOWN on graceful
// Leave, and a BACKEND-FLUSH on Drain (overload).
type Client struct {
	opts Options
	seq  atomic.Uint64

	wrMu sync.Mutex
	wr   *bufio.Writer
}

func New(opts Options) *Client {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.Healthy == nil {
		opts.Healthy = func() bool { return true }
	}
	c := &Client{opts: opts}
	// Seed the heartbeat seq from the process start time (unix seconds) so a
	// backend restart on the SAME IP without a graceful LEAVE resumes ABOVE
	// the seq the director last recorded — otherwise the new process would
	// start at 1, every heartbeat would be rejected as stale, and the backend
	// would stay invisible until the lease expired (a ~TTL blackhole). The
	// seq is per-origin (compared only against this backend's own prior seq,
	// never across nodes), so wall-clock skew between pods is irrelevant.
	c.seq.Store(uint64(time.Now().Unix()))
	return c
}

// Run keeps a registration connection to the director alive until ctx ends,
// reconnecting with capped backoff and heartbeating while healthy.
func (c *Client) Run(ctx context.Context) {
	if c.opts.DirectorAddr == "" {
		slog.Info("backendreg: no director address configured, registration disabled")
		return
	}
	backoff := reconnectBase
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		if err := c.runOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("backendreg: director connection lost, reconnecting", "err", err, "backoff", backoff)
		}
		// A connection that survived well past a heartbeat interval was
		// healthy — reset the backoff so a single later drop (e.g. a director
		// rollout) does not inherit a pegged reconnectMax delay and then leave
		// the backend silent past backend_expire (#787). Without this, backoff
		// doubles to 30s after a few early failures and never recovers.
		if time.Since(start) > 2*c.opts.Interval {
			backoff = reconnectBase
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff *= 2
			if backoff > reconnectMax {
				backoff = reconnectMax
			}
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	var err error
	if c.opts.TLS != nil {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: c.opts.TLS}).DialContext(ctx, "tcp", c.opts.DirectorAddr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", c.opts.DirectorAddr)
	}
	if err != nil {
		return fmt.Errorf("backendreg: dial: %w", err)
	}
	defer conn.Close()

	rd := bufio.NewReaderSize(conn, 4096)
	// Consume the director's server handshake (VERSION / HOST-HAND* / DONE).
	for {
		line, rErr := rd.ReadString('\n')
		if rErr != nil {
			return fmt.Errorf("backendreg: handshake read: %w", rErr)
		}
		if line == "DONE\n" || line == "DONE" {
			break
		}
	}
	// Send our client handshake, then take over the write side.
	wr := bufio.NewWriter(conn)
	if _, err := fmt.Fprintf(wr, "VERSION\tyarilo-director\t1\t0\nME\t%s\t%d\t0\nDONE\n", c.opts.SelfIP, c.opts.Port); err != nil {
		return fmt.Errorf("backendreg: handshake send: %w", err)
	}
	if err := wr.Flush(); err != nil {
		return fmt.Errorf("backendreg: handshake flush: %w", err)
	}
	c.wrMu.Lock()
	c.wr = wr
	c.wrMu.Unlock()
	defer func() {
		c.wrMu.Lock()
		c.wr = nil
		c.wrMu.Unlock()
	}()

	// Read the server side: reply PONG to the director's PING keepalive
	// (#787 — the director closes any client that does not PONG within
	// PingTimeout, so a silent drain loop gets this registration killed every
	// ~30s and the live backend flaps through TTL expiry). Everything else is
	// ignored; a director-closed connection surfaces as a read error that
	// triggers reconnect.
	readErr := make(chan error, 1)
	go func() {
		for {
			line, e := rd.ReadString('\n')
			if e != nil {
				readErr <- e
				return
			}
			if strings.TrimRight(line, "\r\n") == "PING" {
				c.send("PONG")
			}
		}
	}()

	// Heartbeat immediately, then on the interval.
	c.heartbeat()
	t := time.NewTicker(c.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-readErr:
			return fmt.Errorf("backendreg: read: %w", e)
		case <-t.C:
			c.heartbeat()
		}
	}
}

// heartbeat sends one seq'd BACKEND-UP if healthy. An unhealthy backend
// simply goes silent — the director expires its lease.
func (c *Client) heartbeat() {
	if !c.opts.Healthy() {
		slog.Warn("backendreg: unhealthy, skipping heartbeat (director will expire the lease)")
		return
	}
	seq := c.seq.Add(1)
	c.send(fmt.Sprintf("BACKEND-UP\t%s\t%d\t%s\t%d\t%d",
		c.opts.SelfIP, c.opts.Port, c.opts.Tag, c.opts.Vhosts, seq))
}

// Leave sends a graceful BACKEND-DOWN (SIGTERM) so the director removes this
// backend immediately (LEAVE = remove + rehash) without waiting out the TTL.
func (c *Client) Leave() {
	c.send(fmt.Sprintf("BACKEND-DOWN\t%s", c.opts.SelfIP))
}

// Drain sends BACKEND-FLUSH (overload): the backend STAYS in the ring, new
// lookups stop landing on it, existing sessions keep — NO rehash.
func (c *Client) Drain() {
	c.send(fmt.Sprintf("BACKEND-FLUSH\t%s", c.opts.SelfIP))
}

func (c *Client) send(line string) {
	c.wrMu.Lock()
	defer c.wrMu.Unlock()
	if c.wr == nil {
		return
	}
	_, _ = fmt.Fprintln(c.wr, line)
	_ = c.wr.Flush()
}
