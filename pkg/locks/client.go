package locks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Client is the Locker implementation talking the TAB-delimited wire protocol.
// One control connection per Client, shared across goroutines via mu. SUBSCRIBE
// opens a new dedicated connection (returned to caller via the channel).
//
// Owner convention: callers should pass "<process>/<pid>/<sessionID>" so the
// BUSY response identifies the contending peer in logs. Not enforced.
type Client struct {
	dial Dialer

	mu     sync.Mutex
	conn   net.Conn
	reader *reader

	closeOnce sync.Once
	closed    chan struct{}
}

// Dialer opens a fresh connection to the locks server. Used for both the
// control conn at NewClient time and per-subscription conns.
type Dialer func(ctx context.Context) (net.Conn, error)

// DialUnix returns a Dialer for embedded mode (Unix socket).
func DialUnix(path string) Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", path)
		if err != nil {
			return nil, fmt.Errorf("locks/client: dial unix %q: %w", path, err)
		}
		return c, nil
	}
}

// DialTLS returns a Dialer for remote mode (mTLS TCP).
func DialTLS(addr string, tlsCfg *tls.Config) Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		if tlsCfg == nil {
			return nil, fmt.Errorf("locks/client: nil tls config for remote mode")
		}
		d := tls.Dialer{Config: tlsCfg}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("locks/client: dial tls %q: %w", addr, err)
		}
		return c, nil
	}
}

// NewClient opens a control connection and performs the version handshake.
// The returned Client owns the connection — call Close to release it.
func NewClient(ctx context.Context, dial Dialer) (*Client, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	c := &Client{
		dial:   dial,
		conn:   conn,
		reader: newReader(conn),
		closed: make(chan struct{}),
	}
	if err := c.handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) handshake() error {
	if err := writeFields(c.conn, cmdVersion, protocolVersion); err != nil {
		return fmt.Errorf("locks/client: send version: %w", err)
	}
	fields, err := c.reader.readFields()
	if err != nil {
		return fmt.Errorf("locks/client: read version: %w", err)
	}
	if len(fields) < 3 || fields[0] != cmdVersion || fields[1] != protocolVersion || fields[2] != respOK {
		return fmt.Errorf("locks/client: handshake mismatch %v: %w", fields, ErrProtocol)
	}
	return nil
}

// reconnect drops the current conn and opens a new one. Caller holds c.mu.
func (c *Client) reconnect(ctx context.Context) error {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.conn = conn
	c.reader = newReader(conn)
	if err := c.handshake(); err != nil {
		_ = conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// roundtrip serializes a single command/response on the control conn. On
// transient transport error it reconnects once and retries — but only for
// idempotent commands and before any state mutation succeeded server-side.
func (c *Client) roundtrip(ctx context.Context, retry bool, cmd ...string) ([]string, error) {
	select {
	case <-c.closed:
		return nil, ErrClosed
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.reconnect(ctx); err != nil {
			return nil, fmt.Errorf("locks/client: reconnect: %w", err)
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}
	if err := writeFields(c.conn, cmd...); err != nil {
		if retry && isTransport(err) {
			if rerr := c.reconnect(ctx); rerr == nil {
				if err = writeFields(c.conn, cmd...); err == nil {
					return c.reader.readFields()
				}
			}
		}
		return nil, fmt.Errorf("locks/client: write: %w", err)
	}
	fields, err := c.reader.readFields()
	if err != nil {
		if retry && isTransport(err) {
			if rerr := c.reconnect(ctx); rerr == nil {
				if err = writeFields(c.conn, cmd...); err == nil {
					return c.reader.readFields()
				}
			}
		}
		return nil, fmt.Errorf("locks/client: read: %w", err)
	}
	return fields, nil
}

func isTransport(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var nerr net.Error
	return errors.As(err, &nerr)
}

// Lock implements Locker.
func (c *Client) Lock(ctx context.Context, resource, owner string, ttl time.Duration) (Lock, error) {
	ttlStr, err := formatTTL(ttl)
	if err != nil {
		return Lock{}, err
	}
	expires := time.Now().Add(ttl)
	resp, err := c.roundtrip(ctx, true, cmdLock, resource, owner, ttlStr)
	if err != nil {
		return Lock{}, err
	}
	if len(resp) == 0 {
		return Lock{}, fmt.Errorf("locks/client: empty lock response: %w", ErrProtocol)
	}
	switch resp[0] {
	case respOK:
		if len(resp) != 2 {
			return Lock{}, fmt.Errorf("locks/client: malformed OK response: %w", ErrProtocol)
		}
		return Lock{ID: resp[1], Resource: resource, Owner: owner, ExpiresAt: expires}, nil
	case respBusy:
		current := ""
		if len(resp) > 1 {
			current = resp[1]
		}
		return Lock{Resource: resource, Owner: current}, ErrBusy
	case respError:
		return Lock{}, fmt.Errorf("locks/client: server error: %s", strings.Join(resp[1:], " "))
	}
	return Lock{}, fmt.Errorf("locks/client: unexpected response %v: %w", resp, ErrProtocol)
}

// Unlock implements Locker.
func (c *Client) Unlock(ctx context.Context, lockID string) error {
	resp, err := c.roundtrip(ctx, true, cmdUnlock, lockID)
	if err != nil {
		return err
	}
	if len(resp) == 0 {
		return fmt.Errorf("locks/client: empty unlock response: %w", ErrProtocol)
	}
	switch resp[0] {
	case respOK:
		return nil
	case respNotFound:
		return ErrNotFound
	case respError:
		return fmt.Errorf("locks/client: server error: %s", strings.Join(resp[1:], " "))
	}
	return fmt.Errorf("locks/client: unexpected response %v: %w", resp, ErrProtocol)
}

// Renew implements Locker.
func (c *Client) Renew(ctx context.Context, lockID string, ttl time.Duration) error {
	ttlStr, err := formatTTL(ttl)
	if err != nil {
		return err
	}
	resp, err := c.roundtrip(ctx, true, cmdRenew, lockID, ttlStr)
	if err != nil {
		return err
	}
	if len(resp) == 0 {
		return fmt.Errorf("locks/client: empty renew response: %w", ErrProtocol)
	}
	switch resp[0] {
	case respOK:
		return nil
	case respExpired:
		return ErrExpired
	case respError:
		return fmt.Errorf("locks/client: server error: %s", strings.Join(resp[1:], " "))
	}
	return fmt.Errorf("locks/client: unexpected response %v: %w", resp, ErrProtocol)
}

// Emit implements Locker.
func (c *Client) Emit(ctx context.Context, resource string, t EventType, payload string) error {
	resp, err := c.roundtrip(ctx, true, cmdEmit, resource, string(t), payload)
	if err != nil {
		return err
	}
	if len(resp) == 0 || resp[0] != respOK {
		return fmt.Errorf("locks/client: unexpected emit response %v: %w", resp, ErrProtocol)
	}
	return nil
}

// Subscribe implements Locker. Opens a dedicated connection for the
// subscription so it does not block the control conn. The returned channel
// is closed when ctx is cancelled, the conn drops, or the server closes.
func (c *Client) Subscribe(ctx context.Context, resource string) (<-chan Event, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := writeFields(conn, cmdVersion, protocolVersion); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("locks/client: subscribe handshake: %w", err)
	}
	rd := newReader(conn)
	hs, err := rd.readFields()
	if err != nil || len(hs) < 3 || hs[2] != respOK {
		_ = conn.Close()
		return nil, fmt.Errorf("locks/client: subscribe handshake failed: %w", ErrProtocol)
	}
	if err := writeFields(conn, cmdSubscribe, resource); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("locks/client: subscribe send: %w", err)
	}
	ack, err := rd.readFields()
	if err != nil || len(ack) == 0 || ack[0] != respOK {
		_ = conn.Close()
		return nil, fmt.Errorf("locks/client: subscribe not accepted: %w", ErrProtocol)
	}
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer func() { _ = conn.Close() }()
		go func() {
			<-ctx.Done()
			_ = conn.Close()
		}()
		for {
			fields, err := rd.readFields()
			if err != nil {
				return
			}
			if len(fields) < 4 || fields[0] != respEvent {
				continue
			}
			select {
			case out <- Event{Resource: fields[1], Type: EventType(fields[2]), Payload: fields[3]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Close implements Locker. Idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
	return nil
}

// WithLock acquires a lock on resource, starts a background renew loop, runs
// fn while the lock is held, and releases on exit. The renew cadence should
// be a small fraction of ttl (typically ttl/3).
//
// fn receives a context that is cancelled if renewal fails — callers must
// honour the context and abort partially-completed writes when it fires.
//
// Errors from Lock or Unlock propagate as-is. Errors from fn are returned
// after the lock is released. Errors from Renew abort fn (via context) and
// surface as the function's return value.
func WithLock(ctx context.Context, l Locker, resource, owner string, ttl, renewEvery time.Duration, fn func(context.Context) error) error {
	if renewEvery <= 0 || renewEvery >= ttl {
		return fmt.Errorf("locks/withlock: renewEvery %v must be in (0, ttl=%v)", renewEvery, ttl)
	}
	lock, err := l.Lock(ctx, resource, owner, ttl)
	if err != nil {
		return err
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewErr := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(renewEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := l.Renew(ctx, lock.ID, ttl); err != nil {
					renewErr <- err
					cancel()
					return
				}
			case <-stop:
				return
			case <-workCtx.Done():
				return
			}
		}
	}()
	fnErr := fn(workCtx)
	close(stop)
	// Try to release; ignore NotFound (TTL already reclaimed).
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = l.Unlock(releaseCtx, lock.ID)
	releaseCancel()
	select {
	case rerr := <-renewErr:
		return fmt.Errorf("locks/withlock: renew failed: %w", rerr)
	default:
	}
	return fnErr
}
