package authclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// Pool keeps a small number of master-protocol connections open for userdb
// lookups, instead of dialling one per lookup.
//
// The dial is about seven times the work it carries: 3.1ms of TCP, TLS and the
// master's greeting against a 0.44ms USER lookup, measured against the real
// service (#1402). It is also a connection per lookup arriving at yarilo-auth
// -- ~10/s from a modest sandbox load, each with a full handshake on the
// server side too, which is a cost that lands on the dependency rather than on
// us.
//
// Modelled on pkg/locks: fixed slots handed out through a channel, so a slot
// is owned exclusively by whoever holds it and lookups on one connection stay
// serialised, which the master protocol requires.
type Pool struct {
	addr   string
	tlsCfg *tls.Config

	idle        chan *poolSlot
	idleTimeout time.Duration

	closeOnce sync.Once
	closed    chan struct{}
}

type poolSlot struct {
	c        *Client
	lastUsed time.Time
}

// probeDivisor derives the "this has been idle long enough to be suspect"
// threshold from the eviction period, rather than adding a knob nobody would
// tune independently: a connection idle for a quarter of its lifetime is
// probed before it is handed out.
//
// Not probed on every lookup, deliberately. A probe is a round trip, and a
// round trip per lookup would return half of what the pool saves. The
// assumption this encodes: a connection used within the last idleTimeout/4 is
// almost certainly still alive, and if it is not, the redial in exchange()
// covers it at the cost of one failed request.
const probeDivisor = 4

// NewPool returns a pool of size connections to addr. A size of zero or less
// is not an error and not a pool: callers get a fresh connection per lookup,
// which is the behaviour this replaces and the rollback for it.
//
// Nothing is dialled here. A pod that starts while auth is rolling must not
// fail because of it (#1369); the first lookup pays for the first connection.
func NewPool(addr string, tlsCfg *tls.Config, size int, idleTimeout time.Duration) *Pool {
	p := &Pool{
		addr:        addr,
		tlsCfg:      tlsCfg,
		idleTimeout: idleTimeout,
		closed:      make(chan struct{}),
	}
	if size < 1 {
		return p
	}
	p.idle = make(chan *poolSlot, size)
	for i := 0; i < size; i++ {
		p.idle <- &poolSlot{}
	}
	return p
}

// Userdb resolves username, reusing a pooled connection when one is available.
func (p *Pool) Userdb(ctx context.Context, username string) (*protocol.UserInfo, error) {
	select {
	case <-p.closed:
		return nil, ErrClosed
	default:
	}

	// No pool configured: the old shape, one connection per lookup.
	if p.idle == nil {
		c, err := DialContext(ctx, p.addr, p.tlsCfg)
		if err != nil {
			return nil, err
		}
		defer c.Close() //nolint:errcheck
		return c.Userdb(ctx, username)
	}

	var slot *poolSlot
	select {
	case <-p.closed:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, waitFailure(ctx.Err())
	case slot = <-p.idle:
	}
	defer func() { p.idle <- slot }()

	if err := p.prepare(ctx, slot); err != nil {
		return nil, err
	}
	ui, err := slot.c.Userdb(ctx, username)
	if err != nil && isConnectionError(err) {
		// The client redials once on a transport failure of its own. Reaching
		// here means that failed too, so the connection is not reusable --
		// dropped rather than handed back, and NOT retried across the other
		// slots: a lookup that walks every connection turns "auth is down"
		// into "this request hangs", which is the answer we spent #1339 and
		// #1402 making sure a client does not get.
		slot.drop()
		return nil, err
	}
	slot.lastUsed = time.Now()
	return ui, err
}

// prepare makes the slot usable: fresh connection, evicted if too old, probed
// if it has been quiet long enough to be doubted.
func (p *Pool) prepare(ctx context.Context, slot *poolSlot) error {
	if slot.c != nil && p.idleTimeout > 0 && time.Since(slot.lastUsed) >= p.idleTimeout {
		slot.drop()
	}
	if slot.c != nil && p.probeThreshold() > 0 && time.Since(slot.lastUsed) >= p.probeThreshold() {
		if err := slot.c.Ping(ctx); err != nil {
			slot.drop()
		}
	}
	if slot.c != nil {
		return nil
	}
	c, err := DialContext(ctx, p.addr, p.tlsCfg)
	if err != nil {
		return err
	}
	slot.c = c
	slot.lastUsed = time.Now()
	return nil
}

func (p *Pool) probeThreshold() time.Duration {
	if p.idleTimeout <= 0 {
		return 0
	}
	return p.idleTimeout / probeDivisor
}

func (s *poolSlot) drop() {
	if s.c != nil {
		_ = s.c.Close()
		s.c = nil
	}
}

// Close releases every pooled connection. Idempotent.
func (p *Pool) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.closed)
		if p.idle == nil {
			return
		}
		for i := 0; i < cap(p.idle); i++ {
			select {
			case slot := <-p.idle:
				if slot.c != nil {
					if cerr := slot.c.Close(); cerr != nil && err == nil {
						err = cerr
					}
					slot.c = nil
				}
			case <-time.After(time.Second):
				// A slot still in use by an in-flight lookup. Its connection
				// closes with the process; waiting for it would hold shutdown
				// on a request that may be blocked on a dependency that is
				// itself down.
				err = errors.Join(err, fmt.Errorf("authclient: pool close: %d connection(s) still in use", cap(p.idle)-i))
				return
			}
		}
	})
	return err
}
