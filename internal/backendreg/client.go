// Package backendreg self-registers a backend with the director and
// heartbeats, so the ring reflects live backend pods.
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
	// DirectorAddr is "host:port"; any director replica works, the
	// registration is gossiped ring-wide.
	DirectorAddr string
	// SelfIP/Port/Tag identify this backend in the ring; Port is the
	// session port. Vhosts is the ring weight (0 = default 100).
	SelfIP string
	Port   int
	Tag    string
	Vhosts int
	// Interval paces the heartbeat. 0 = 10s.
	Interval time.Duration
	// Healthy gates the heartbeat; an unhealthy backend goes silent and
	// its lease expires ring-wide.
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
	// seed seq from unix time so a restart on the same IP without a
	// graceful LEAVE resumes above the director's last recorded seq;
	// seq is per-origin, so cross-pod clock skew doesn't matter
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
		// a long-lived connection was healthy: reset backoff so a later
		// drop doesn't inherit a pegged reconnectMax delay
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
	// consume the director's handshake (VERSION / HOST-HAND* / DONE)
	for {
		line, rErr := rd.ReadString('\n')
		if rErr != nil {
			return fmt.Errorf("backendreg: handshake read: %w", rErr)
		}
		if line == "DONE\n" || line == "DONE" {
			break
		}
	}
	// send our handshake, then take over the write side
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

	// reply PONG to the director's PING keepalive — the director closes
	// clients that don't PONG within PingTimeout. everything else is
	// ignored; a closed connection surfaces as a read error.
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

	// heartbeat immediately, then on the interval
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

// Leave sends BACKEND-DOWN so the director removes this backend
// immediately (remove + rehash) instead of waiting out the TTL.
func (c *Client) Leave() {
	c.send(fmt.Sprintf("BACKEND-DOWN\t%s", c.opts.SelfIP))
}

// Drain sends BACKEND-FLUSH: the backend stays in the ring, new lookups
// stop landing on it, existing sessions keep; no rehash.
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
