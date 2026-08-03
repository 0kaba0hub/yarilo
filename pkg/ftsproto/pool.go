package ftsproto

import (
	"errors"
	"fmt"
	"time"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// ErrPoolExhausted is returned when no connection became free within the wait
// budget. It is a distinct error so a caller can tell "the service is busy"
// from "the service failed".
var ErrPoolExhausted = errors.New("ftsproto: no free connection")

// Pool is a Client backed by several reconnecting connections.
//
// A single Remote serialises request/response pairs on one socket, so a caller
// that starts N goroutines against one gets N queued calls, not N concurrent
// ones. The server has always handled each connection on its own goroutine;
// the client side was the bottleneck. A pool is therefore the thing that makes
// concurrency exist at all — a limiter over a single connection would only
// have bounded a queue.
//
// Connections are opened on demand, not at construction: a pool of eight costs
// nothing until eight calls actually overlap.
type Pool struct {
	free chan *Lazy
	all  []*Lazy
	// wait bounds how long a caller queues for a free connection. It reuses the
	// dial timeout rather than introducing a second number: both answer "how
	// long is it reasonable to wait to reach fts".
	wait time.Duration
}

// NewPool returns a Client holding size connections to addr. A size below one
// is treated as one, which behaves exactly like a single connection.
func NewPool(addr string, size int, dialTimeout time.Duration) *Pool {
	if size < 1 {
		size = 1
	}
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	p := &Pool{free: make(chan *Lazy, size), wait: dialTimeout}
	for i := 0; i < size; i++ {
		c := NewLazy(addr, dialTimeout)
		p.all = append(p.all, c)
		p.free <- c
	}
	return p
}

// Size reports how many connections the pool may use.
func (p *Pool) Size() int { return len(p.all) }

func (p *Pool) acquire() (*Lazy, error) {
	// The common case is a free connection, taken without arming a timer.
	select {
	case c := <-p.free:
		return c, nil
	default:
	}
	t := time.NewTimer(p.wait)
	defer t.Stop()
	select {
	case c := <-p.free:
		return c, nil
	case <-t.C:
		return nil, fmt.Errorf("%w after %s (pool size %d)", ErrPoolExhausted, p.wait, len(p.all))
	}
}

// release returns a connection. It never blocks: the channel has exactly one
// slot per connection, so a released one always fits.
func (p *Pool) release(c *Lazy) { p.free <- c }

func (p *Pool) do(fn func(*Lazy) error) error {
	c, err := p.acquire()
	if err != nil {
		return err
	}
	defer p.release(c)
	return fn(c)
}

// Close closes every connection. Calls in flight keep their connection until
// they finish; Close does not wait for them.
func (p *Pool) Close() error {
	var firstErr error
	for _, c := range p.all {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Pool) Index(user string, m fts.MailboxRef, maxUID uint32, maxRecent int) error {
	return p.do(func(c *Lazy) error { return c.Index(user, m, maxUID, maxRecent) })
}

func (p *Pool) Prepend(user string, m fts.MailboxRef, maxUID uint32) error {
	return p.do(func(c *Lazy) error { return c.Prepend(user, m, maxUID) })
}

func (p *Pool) Expunge(user string, m fts.MailboxRef, uid uint32) error {
	return p.do(func(c *Lazy) error { return c.Expunge(user, m, uid) })
}

func (p *Pool) Lookup(user string, m fts.MailboxRef, q fts.Query) (fts.Result, error) {
	var res fts.Result
	err := p.do(func(c *Lazy) error {
		var e error
		res, e = c.Lookup(user, m, q)
		return e
	})
	return res, err
}

func (p *Pool) Status(user string, m fts.MailboxRef) (uint32, uint32, error) {
	var last, sum uint32
	err := p.do(func(c *Lazy) error {
		var e error
		last, sum, e = c.Status(user, m)
		return e
	})
	return last, sum, err
}

func (p *Pool) Rescan(user string, m fts.MailboxRef) error {
	return p.do(func(c *Lazy) error { return c.Rescan(user, m) })
}

func (p *Pool) Optimize(user string) error {
	return p.do(func(c *Lazy) error { return c.Optimize(user) })
}
