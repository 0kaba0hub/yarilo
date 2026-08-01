package warden

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPoolSize is the number of long-lived connections a Pool keeps. The
// wire protocol carries no request id, so a connection holds one request for a
// single round trip (#885); a handful suffices since each op is a sub-ms RPC.
const DefaultPoolSize = 4

// Pool is a fixed set of long-lived connections to yarilo-warden, each guarded
// by its own mutex.
//
// Sessions do NOT own a connection: every command carries the session id on the
// wire and the server keeps no per-connection state, so a connection is freely
// shared. Losing one redials in a few hundred ms, well within the 90s session
// TTL. See internal/warden/shared_conn_test.go for the pinned invariants.
type Pool struct {
	addr    string
	tlsCfg  *tls.Config
	timeout time.Duration

	conns []*pooledConn
	next  atomic.Uint64

	closeOnce sync.Once
	closed    atomic.Bool
}

// pooledConn is one connection and the mutex serialising its round trips.
type pooledConn struct {
	mu sync.Mutex
	c  *Conn // nil until first use, and after a transport error
}

// NewPool creates a Pool of size connections against addr, dialled lazily on
// first use so it can be built before yarilo-warden is reachable. size <= 0
// selects DefaultPoolSize.
func NewPool(addr string, tlsCfg *tls.Config, size int, timeout time.Duration) *Pool {
	if size <= 0 {
		size = DefaultPoolSize
	}
	p := &Pool{addr: addr, tlsCfg: tlsCfg, timeout: timeout, conns: make([]*pooledConn, size)}
	for i := range p.conns {
		p.conns[i] = &pooledConn{}
	}
	return p
}

// Size reports how many connections the pool holds.
func (p *Pool) Size() int { return len(p.conns) }

// Close closes every connection. Safe to call once; further calls are no-ops.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		for _, pc := range p.conns {
			pc.mu.Lock()
			if pc.c != nil {
				pc.c.Close()
				pc.c = nil
			}
			pc.mu.Unlock()
		}
	})
}

// do runs fn on one connection, redialling and retrying once on a dead
// connection. The retry is safe because every op is idempotent in its session
// id (a repeated CONNECT upserts the same record). ErrTooManyConns is a
// protocol answer, not a transport failure, so it never triggers a redial.
func (p *Pool) do(fn func(*Conn) error) error {
	if p.closed.Load() {
		return errors.New("warden/pool: closed")
	}
	pc := p.conns[int(p.next.Add(1)-1)%len(p.conns)]

	pc.mu.Lock()
	defer pc.mu.Unlock()

	for attempt := range 2 {
		if pc.c == nil {
			c, err := Dial(p.addr, p.tlsCfg, p.timeout)
			if err != nil {
				return err
			}
			pc.c = c
		}
		err := fn(pc.c)
		if err == nil || errors.Is(err, ErrTooManyConns) {
			return err
		}
		// Transport failure: discard the connection and retry once on a fresh one.
		pc.c.Close()
		pc.c = nil
		if attempt == 1 {
			return err
		}
	}
	return nil
}

// Connect registers a session. Returns ErrTooManyConns when the server refuses
// it because the (user, ip) pair is at its limit.
func (p *Pool) Connect(id, user, ip, service string) error {
	return p.do(func(c *Conn) error { return c.Connect(id, user, ip, service) })
}

// Disconnect deregisters a session and releases its accounting slot.
func (p *Pool) Disconnect(id, user, ip, service string) error {
	return p.do(func(c *Conn) error { return c.Disconnect(id, user, ip, service) })
}

// Backend records the backend pod IP a session was routed to (#814).
func (p *Pool) Backend(id, backendIP string) error {
	return p.do(func(c *Conn) error { return c.Backend(id, backendIP) })
}

// Select records the currently-SELECTed mailbox for a session.
func (p *Pool) Select(id, folder string) error {
	return p.do(func(c *Conn) error { return c.Select(id, folder) })
}

// PenaltyLookup / PenaltyUpdate run the auth-penalty ops over the pool so they
// survive a warden restart (#946): p.do redials and retries once. Satisfies
// protocol.PenaltyStore.
func (p *Pool) PenaltyLookup(ip string) (int, error) {
	var count int
	err := p.do(func(c *Conn) error {
		var lerr error
		count, lerr = c.PenaltyLookup(ip)
		return lerr
	})
	return count, err
}

func (p *Pool) PenaltyUpdate(ip string, count int) error {
	return p.do(func(c *Conn) error { return c.PenaltyUpdate(ip, count) })
}

// Heartbeat renews a session's TTL. Reports false when the server does not know
// the session (reaped); the caller must then re-issue CONNECT.
func (p *Pool) Heartbeat(id string) (bool, error) {
	var known bool
	err := p.do(func(c *Conn) error {
		var herr error
		known, herr = c.Heartbeat(id)
		return herr
	})
	return known, err
}

// HeartbeatLoop renews id every interval until ctx is cancelled, over the
// shared pool. Each beat holds a connection only for its round trip. Returns
// nil on ctx cancellation; on unknown session it calls onUnknown and returns
// nil.
func (p *Pool) HeartbeatLoop(ctx context.Context, id string, interval time.Duration, onUnknown func()) error {
	if interval <= 0 {
		return fmt.Errorf("warden/pool: heartbeat interval must be > 0")
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			known, err := p.Heartbeat(id)
			if err != nil {
				return err
			}
			if !known {
				if onUnknown != nil {
					onUnknown()
				}
				return nil
			}
		}
	}
}
