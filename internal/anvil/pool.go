package anvil

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
// anvil wire protocol carries no request id, so a connection cannot be
// multiplexed the way yarilo-auth's is (#885) — each request holds its
// connection for one round trip. Every operation is a sub-millisecond in-cluster
// RPC, so a handful of connections serves tens of thousands of operations per
// second, and the point is simply that the count no longer tracks the login rate.
const DefaultPoolSize = 4

// Pool is a fixed set of long-lived connections to yarilo-anvil, each guarded by
// its own mutex.
//
// Sessions do NOT own a connection. Every command carries the session id on the
// wire (CONNECT/DISCONNECT/HEARTBEAT/SELECT/BACKEND all take it), and the server
// keeps no per-connection state — its handleConn performs no cleanup when a
// connection drops, and the connection-limit accounting keys on the (user, ip)
// pair from the CONNECT arguments rather than on connection identity. That is
// what makes sharing safe; see internal/anvil/shared_conn_test.go, which pins
// those invariants.
//
// The trade-off this introduces: losing one connection now affects every session
// using it rather than one. Recovery is a redial of a few hundred milliseconds
// against a 90s session TTL, so the sweeper never sees a gap, and
// yarilo_anvil_sessions_reaped_total makes it visible if that ever stops holding.
type Pool struct {
	addr    string
	tlsCfg  *tls.Config
	timeout time.Duration

	conns []*pooledConn
	next  atomic.Uint64

	closeOnce sync.Once
	closed    atomic.Bool
}

// pooledConn is one connection plus the mutex serialising its round trips.
type pooledConn struct {
	mu sync.Mutex
	c  *Conn // nil until first use, and after a transport error
}

// NewPool creates a Pool of size connections against addr. Connections are
// dialled lazily on first use, so the pool can be constructed before
// yarilo-anvil is reachable. size <= 0 selects DefaultPoolSize.
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

// do runs fn on one connection, redialling and retrying once if the connection
// turned out to be dead.
//
// The retry is safe because every anvil operation is idempotent in its session
// id: a repeated CONNECT for the same id upserts the same session record, and
// HEARTBEAT/SELECT/BACKEND/DISCONNECT are naturally so. ErrTooManyConns is a
// protocol answer rather than a transport failure, so it is returned untouched
// and never triggers a redial.
func (p *Pool) do(fn func(*Conn) error) error {
	if p.closed.Load() {
		return errors.New("anvil/pool: closed")
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
		// Transport failure: discard the connection so the next caller redials,
		// and retry once on a fresh one.
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

// Heartbeat renews a session's TTL. Reports false when the server does not know
// the session — it was reaped, and the caller must re-issue CONNECT.
func (p *Pool) Heartbeat(id string) (bool, error) {
	var known bool
	err := p.do(func(c *Conn) error {
		var herr error
		known, herr = c.Heartbeat(id)
		return herr
	})
	return known, err
}

// HeartbeatLoop renews id every interval until ctx is cancelled, mirroring
// Conn.HeartbeatLoop but over the shared pool. Each beat is one short round trip
// that holds its connection only for that beat, so many sessions share the pool
// without blocking each other for any meaningful time.
//
// Returns nil on ctx cancellation. On unknown session it calls onUnknown and
// returns nil — the session is gone and beating harder will not bring it back.
func (p *Pool) HeartbeatLoop(ctx context.Context, id string, interval time.Duration, onUnknown func()) error {
	if interval <= 0 {
		return fmt.Errorf("anvil/pool: heartbeat interval must be > 0")
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
