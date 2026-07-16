package ftsproto

import (
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/fts"
)

// Lazy is a reconnecting Client: it dials on first use and drops the
// connection on any transport error so the next call redials. Session pods
// wire this so yarilo-fts restarts (or starting order) never wedge them.
type Lazy struct {
	addr    string
	timeout time.Duration

	mu   sync.Mutex
	conn *Remote
}

// NewLazy returns a client for addr; no connection is made until first use.
func NewLazy(addr string, dialTimeout time.Duration) *Lazy {
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	return &Lazy{addr: addr, timeout: dialTimeout}
}

func (l *Lazy) get() (*Remote, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		return l.conn, nil
	}
	c, err := Dial(l.addr, l.timeout)
	if err != nil {
		return nil, err
	}
	l.conn = c
	return c, nil
}

func (l *Lazy) drop(c *Remote) {
	l.mu.Lock()
	if l.conn == c {
		l.conn = nil
	}
	l.mu.Unlock()
	c.Close() //nolint:errcheck
}

// do runs one call, dropping the connection on error so the next call
// redials. Server-side NO replies are returned as errors WITHOUT dropping
// (the transport is fine); transport errors drop.
func (l *Lazy) do(fn func(*Remote) error) error {
	c, err := l.get()
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		if !isServerReply(err) {
			l.drop(c)
		}
		return err
	}
	return nil
}

func isServerReply(err error) bool {
	// Remote.call wraps NO replies with the "ftsproto: server:" prefix.
	return err != nil && len(err.Error()) >= 17 && err.Error()[:17] == "ftsproto: server:"
}

func (l *Lazy) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		err := l.conn.Close()
		l.conn = nil
		return err
	}
	return nil
}

func (l *Lazy) Index(user string, m fts.MailboxRef, maxUID uint32, maxRecent int) error {
	return l.do(func(c *Remote) error { return c.Index(user, m, maxUID, maxRecent) })
}

func (l *Lazy) Prepend(user string, m fts.MailboxRef, maxUID uint32) error {
	return l.do(func(c *Remote) error { return c.Prepend(user, m, maxUID) })
}

func (l *Lazy) Expunge(user string, m fts.MailboxRef, uid uint32) error {
	return l.do(func(c *Remote) error { return c.Expunge(user, m, uid) })
}

func (l *Lazy) Lookup(user string, m fts.MailboxRef, q fts.Query) (fts.Result, error) {
	var res fts.Result
	err := l.do(func(c *Remote) error {
		var e error
		res, e = c.Lookup(user, m, q)
		return e
	})
	return res, err
}

func (l *Lazy) Status(user string, m fts.MailboxRef) (uint32, uint32, error) {
	var last, sum uint32
	err := l.do(func(c *Remote) error {
		var e error
		last, sum, e = c.Status(user, m)
		return e
	})
	return last, sum, err
}

func (l *Lazy) Rescan(user string, m fts.MailboxRef) error {
	return l.do(func(c *Remote) error { return c.Rescan(user, m) })
}

func (l *Lazy) Optimize(user string) error {
	return l.do(func(c *Remote) error { return c.Optimize(user) })
}
