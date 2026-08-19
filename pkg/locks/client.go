package locks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultPoolSize is the number of concurrent control connections per Client.
// Each connection handles one round-trip at a time; the pool lets goroutines
// proceed in parallel instead of serialising through a single mutex.
const defaultPoolSize = 16

// connSlot is one entry in the Client connection pool.
type connSlot struct {
	conn   net.Conn
	reader *reader
}

// Client is the Locker implementation talking the TAB-delimited wire protocol.
// It maintains a pool of control connections so concurrent goroutines do not
// serialise; SUBSCRIBE opens a dedicated connection per subscription.
//
// Owner convention (not enforced): callers pass "<process>/<pid>/<sessionID>"
// so the BUSY response identifies the contending peer in logs.
type Client struct {
	dial     Dialer
	poolSize int

	// idle is the connection pool: a buffered channel of available slots.
	// Taking a slot gives exclusive access to its conn; returning it makes it
	// available again. Cap = poolSize; starts with all-nil conns (lazy connect).
	idle chan *connSlot

	// holdsMu guards the holds map. Separate from the pool so HoldsResource is
	// safe to call mid-roundtrip.
	//
	// Holds are tracked per-goroutine, so the HoldsResource re-entrancy
	// short-circuit applies only to the goroutine that took the lock; a
	// concurrent goroutine on the same client sees no hold and goes through
	// normal Acquire (ErrBusy + retry until release).
	holdsMu sync.RWMutex
	holds   map[uint64]map[string]string // goID → resource → lockID

	closeOnce sync.Once
	closed    chan struct{}
}

// ClientOption tunes a Client at construction time.
type ClientOption func(*Client)

// WithPoolSize overrides the number of concurrent control connections.
// The default is 16. Larger values reduce mutex contention under high
// concurrency at the cost of more open TCP connections.
func WithPoolSize(n int) ClientOption {
	return func(c *Client) {
		if n > 0 {
			c.poolSize = n
		}
	}
}

// Dialer opens a fresh connection to the locks server. Used for both pool
// slots and per-subscription conns.
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

// DialTCP returns a Dialer for remote mode over plain TCP (no TLS).
func DialTCP(addr string) Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("locks/client: dial tcp %q: %w", addr, err)
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

// NewClient creates a Client with a lazy-initialised connection pool. The first
// round-trip on each pool slot establishes the connection and performs the
// version handshake. Call Close to release all connections.
func NewClient(ctx context.Context, dial Dialer, opts ...ClientOption) (*Client, error) {
	c := &Client{
		dial:     dial,
		poolSize: defaultPoolSize,
		holds:    make(map[uint64]map[string]string),
		closed:   make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	c.idle = make(chan *connSlot, c.poolSize)
	for i := 0; i < c.poolSize; i++ {
		c.idle <- &connSlot{} // conn == nil → lazy connect on first use
	}
	// Verify the server is reachable by connecting one slot eagerly.
	slot := <-c.idle
	if err := c.ensureConnected(ctx, slot); err != nil {
		c.idle <- slot // return before closing so Close() can drain
		return nil, err
	}
	c.idle <- slot
	return c, nil
}

func (c *Client) handshakeSlot(slot *connSlot) error {
	if err := writeFields(slot.conn, cmdVersion, protocolVersion); err != nil {
		return fmt.Errorf("locks/client: send version: %w", err)
	}
	fields, err := slot.reader.readFields()
	if err != nil {
		return fmt.Errorf("locks/client: read version: %w", err)
	}
	if len(fields) < 3 || fields[0] != cmdVersion || fields[1] != protocolVersion || fields[2] != respOK {
		return fmt.Errorf("locks/client: handshake mismatch %v: %w", fields, ErrProtocol)
	}
	return nil
}

// ensureConnected opens (or reopens) the connection for a pool slot.
// Caller holds exclusive access to the slot (taken from the idle channel).
func (c *Client) ensureConnected(ctx context.Context, slot *connSlot) error {
	if slot.conn != nil {
		return nil
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	slot.conn = conn
	slot.reader = newReader(conn)
	if err := c.handshakeSlot(slot); err != nil {
		_ = conn.Close()
		slot.conn = nil
		slot.reader = nil
		return err
	}
	return nil
}

// reconnectSlot drops the current conn and opens a new one.
// Caller holds exclusive access to the slot.
func (c *Client) reconnectSlot(ctx context.Context, slot *connSlot) error {
	if slot.conn != nil {
		_ = slot.conn.Close()
		slot.conn = nil
		slot.reader = nil
	}
	return c.ensureConnected(ctx, slot)
}

// roundtrip serializes a single command/response on one pool slot, reconnecting
// once on transient transport error. Retry is safe for every command: LOCK is
// the only mutating call and lock IDs are generated client-side, so a retry the
// server already saw cannot collide.
func (c *Client) roundtrip(ctx context.Context, cmd ...string) ([]string, error) {
	select {
	case <-c.closed:
		return nil, ErrClosed
	default:
	}

	// Take an idle slot (blocks until one is available or ctx fires).
	var slot *connSlot
	select {
	case <-c.closed:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	case slot = <-c.idle:
	}
	defer func() { c.idle <- slot }()

	if err := c.ensureConnected(ctx, slot); err != nil {
		return nil, fmt.Errorf("locks/client: connect: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		// The connection is captured, not re-read at return: a failed
		// reconnectSlot below clears slot.conn, and a deferred read of the
		// field would then dereference nil -- which is a panic on the path
		// that runs when the lock service goes away mid-connection, i.e.
		// exactly when it must not (#1336).
		conn := slot.conn
		_ = conn.SetDeadline(deadline)
		defer func() { _ = conn.SetDeadline(time.Time{}) }()
	}
	if err := writeFields(slot.conn, cmd...); err != nil {
		if isTransport(err) {
			if rerr := c.reconnectSlot(ctx, slot); rerr == nil {
				if err = writeFields(slot.conn, cmd...); err == nil {
					return slot.reader.readFields()
				}
			}
		}
		return nil, fmt.Errorf("locks/client: write: %w", err)
	}
	fields, err := slot.reader.readFields()
	if err != nil {
		if isTransport(err) {
			if rerr := c.reconnectSlot(ctx, slot); rerr == nil {
				if err = writeFields(slot.conn, cmd...); err == nil {
					return slot.reader.readFields()
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
	resp, err := c.roundtrip(ctx, cmdLock, resource, owner, ttlStr)
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
		gid := goID()
		c.holdsMu.Lock()
		if c.holds[gid] == nil {
			c.holds[gid] = make(map[string]string)
		}
		c.holds[gid][resource] = resp[1]
		c.holdsMu.Unlock()
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

// LockShared implements Locker.
func (c *Client) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (Lock, error) {
	ttlStr, err := formatTTL(ttl)
	if err != nil {
		return Lock{}, err
	}
	expires := time.Now().Add(ttl)
	resp, err := c.roundtrip(ctx, cmdLockShared, resource, owner, ttlStr)
	if err != nil {
		return Lock{}, err
	}
	if len(resp) == 0 {
		return Lock{}, fmt.Errorf("locks/client: empty lock-shared response: %w", ErrProtocol)
	}
	switch resp[0] {
	case respOK:
		if len(resp) != 2 {
			return Lock{}, fmt.Errorf("locks/client: malformed OK response: %w", ErrProtocol)
		}
		gid := goID()
		c.holdsMu.Lock()
		if c.holds[gid] == nil {
			c.holds[gid] = make(map[string]string)
		}
		c.holds[gid][resource] = resp[1]
		c.holdsMu.Unlock()
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
	resp, err := c.roundtrip(ctx, cmdUnlock, lockID)
	if err != nil {
		return err
	}
	c.dropHoldByID(lockID)
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

// HoldsResource implements Locker. Returns true only when the calling goroutine
// itself holds this resource; concurrent goroutines on the same client see
// false and go through normal Acquire.
func (c *Client) HoldsResource(resource string) bool {
	gid := goID()
	c.holdsMu.RLock()
	defer c.holdsMu.RUnlock()
	if m, ok := c.holds[gid]; ok {
		_, has := m[resource]
		return has
	}
	return false
}

// dropHoldByID removes the calling goroutine's resource→ID entry matching
// lockID. No-op if the ID was not tracked.
func (c *Client) dropHoldByID(lockID string) {
	gid := goID()
	c.holdsMu.Lock()
	defer c.holdsMu.Unlock()
	m, ok := c.holds[gid]
	if !ok {
		return
	}
	for resource, id := range m {
		if id == lockID {
			delete(m, resource)
			if len(m) == 0 {
				delete(c.holds, gid)
			}
			return
		}
	}
}

// goID returns the current goroutine's ID by parsing the runtime.Stack header.
// Used to track lock ownership per goroutine so the HoldsResource re-entrancy
// short-circuit applies only to the goroutine that took the lock.
func goID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// Header is "goroutine 12345 [...]"; skip "goroutine ".
	if n < 10 {
		return 0
	}
	var id uint64
	for _, b := range buf[10:n] {
		if b < '0' || b > '9' {
			break
		}
		id = id*10 + uint64(b-'0')
	}
	return id
}

// Renew implements Locker.
func (c *Client) Renew(ctx context.Context, lockID string, ttl time.Duration) error {
	ttlStr, err := formatTTL(ttl)
	if err != nil {
		return err
	}
	resp, err := c.roundtrip(ctx, cmdRenew, lockID, ttlStr)
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
	resp, err := c.roundtrip(ctx, cmdEmit, resource, string(t), payload)
	if err != nil {
		return err
	}
	if len(resp) == 0 || resp[0] != respOK {
		return fmt.Errorf("locks/client: unexpected emit response %v: %w", resp, ErrProtocol)
	}
	return nil
}

// IncrementCounter implements Locker. Atomically adds delta to the counter at
// key and returns the post-increment value.
func (c *Client) IncrementCounter(ctx context.Context, key string, delta int64) (int64, error) {
	resp, err := c.roundtrip(ctx, cmdCounterInc, key, strconv.FormatInt(delta, 10))
	if err != nil {
		return 0, err
	}
	if len(resp) < 2 || resp[0] != respOK {
		if len(resp) > 0 && resp[0] == respError {
			return 0, fmt.Errorf("locks/client: counter inc: %s", strings.Join(resp[1:], " "))
		}
		return 0, fmt.Errorf("locks/client: unexpected counter response %v: %w", resp, ErrProtocol)
	}
	v, err := strconv.ParseInt(resp[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("locks/client: parse counter value %q: %w", resp[1], err)
	}
	return v, nil
}

// Subscribe implements Locker. Opens a dedicated connection for the
// subscription so it does not consume a pool slot. The returned channel
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

// Close implements Locker. Idempotent. Closes all pool connections.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		// Drain and close all pool slots. Slots that are mid-roundtrip will
		// be returned to the channel after the goroutine detects the closed
		// channel; we only close the ones already idle.
		for i := 0; i < c.poolSize; i++ {
			select {
			case slot := <-c.idle:
				if slot.conn != nil {
					_ = slot.conn.Close()
				}
			default:
				// slot is in-use; its connection will be abandoned when the
				// goroutine returns it after noticing c.closed.
			}
		}
	})
	return nil
}

// Acquire is Lock with blocking semantics: retries on ErrBusy with exponential
// backoff (1ms → 100ms cap, small jitter) until ctx is cancelled or the lock is
// taken. Returns the Lock on success, else the last underlying error.
func Acquire(ctx context.Context, l Locker, resource, owner string, ttl time.Duration) (Lock, error) {
	backoff := time.Millisecond
	const maxBackoff = 100 * time.Millisecond
	for {
		lock, err := l.Lock(ctx, resource, owner, ttl)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrBusy) {
			return Lock{}, err
		}
		// Jitter by ±25% so concurrent retriers do not synchronise.
		jitter := time.Duration(int64(backoff) / 4)
		wait := backoff - jitter + time.Duration(time.Now().UnixNano()%int64(2*jitter+1))
		select {
		case <-ctx.Done():
			return Lock{}, ctx.Err()
		case <-time.After(wait):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// AcquireShared is LockShared with blocking semantics, mirroring Acquire. Use
// it for read-path callers that must block only against an in-flight exclusive
// writer, not against other concurrent readers.
func AcquireShared(ctx context.Context, l Locker, resource, owner string, ttl time.Duration) (Lock, error) {
	backoff := time.Millisecond
	const maxBackoff = 100 * time.Millisecond
	for {
		lock, err := l.LockShared(ctx, resource, owner, ttl)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrBusy) {
			return Lock{}, err
		}
		jitter := time.Duration(int64(backoff) / 4)
		wait := backoff - jitter + time.Duration(time.Now().UnixNano()%int64(2*jitter+1))
		select {
		case <-ctx.Done():
			return Lock{}, ctx.Err()
		case <-time.After(wait):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
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

// NewClientWaiting is NewClient with a bounded wait for the service to come up.
//
// A component starting before yarilo-locks gets "connection refused", which is
// not the same failure as a wrong endpoint: pod start order is not guaranteed
// and the lock service is a separate deployment, so "not up yet" is the
// ordinary case. Exiting on it costs a restart, and spends the RESTARTS counter
// every rollout window is judged by -- a counter is only useful while it reads
// zero (#1350).
//
// Bounded rather than infinite, deliberately: a genuinely misconfigured
// endpoint has to keep failing loudly rather than retry for ever inside a pod
// that looks healthy. wait <= 0 makes this exactly NewClient.
func NewClientWaiting(ctx context.Context, dial Dialer, wait time.Duration, opts ...ClientOption) (*Client, error) {
	c, err := NewClient(ctx, dial, opts...)
	if err == nil || wait <= 0 {
		return c, err
	}
	deadline := time.Now().Add(wait)
	// Said once, at the start of waiting: a line per attempt would bury the
	// startup log of every pod that comes up a second early.
	slog.Warn("locks: service not reachable yet, waiting", "wait", wait, "err", err)
	for delay := 100 * time.Millisecond; ; {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("locks/client: not reachable after %s: %w", wait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
		c, err = NewClient(ctx, dial, opts...)
		if err == nil {
			slog.Info("locks: service reachable, continuing startup")
			return c, nil
		}
	}
}
