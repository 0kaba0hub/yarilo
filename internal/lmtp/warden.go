package lmtp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/internal/warden"
)

// wardenSessionClient is the per-LMTP-session handle to
// yarilo-warden. It owns one TCP connection, tracks every Connect
// the session has issued (so Logout / Reset / failed delivery
// can cleanly Disconnect them), and gates per-recipient concurrency
// against the configured limit.
//
// Lazy: the underlying warden.Conn is dialled on the first call.
// A dial failure short-circuits the limit check (returns
// ErrWardenUnavailable) so the LMTP server can decide whether to
// fall back to "no limit, just deliver" or hard-reject — current
// behaviour is the former.
type wardenSessionClient struct {
	addr    string
	limit   int    // SoT for the cluster-wide concurrency check; -1 = unlimited
	service string // "lmtp"
	peerIP  string

	mu        sync.Mutex
	conn      *warden.Conn
	dialErr   error
	connected []wardenEntry // outstanding CONNECTs awaiting DISCONNECT
}

// wardenEntry tracks one in-flight Connect so the matching
// Disconnect can fire later. id is the session-unique handle the
// warden server keys responses on; we generate it locally per
// CONNECT call.
type wardenEntry struct {
	id   string
	user string
}

// ErrWardenUnavailable is returned by the concurrency check when
// the warden server cannot be reached. Callers downgrade to
// best-effort (deliver without the limit) and log a warning.
var ErrWardenUnavailable = errors.New("lmtp: warden unavailable")

// ErrTooManyConcurrent is returned by reserveDelivery when the
// recipient is at or above lmtp_user_concurrency_limit. The
// caller surfaces it as `451 4.3.0 Too many concurrent
// deliveries for user`.
var ErrTooManyConcurrent = errors.New("lmtp: too many concurrent deliveries for user")

// newWardenSessionClient wires a session client; the connection
// is not opened until the first reserveDelivery call.
func newWardenSessionClient(addr string, limit int, peerIP string) *wardenSessionClient {
	return &wardenSessionClient{
		addr:    addr,
		limit:   limit,
		service: "lmtp",
		peerIP:  peerIP,
	}
}

// reserveDelivery performs LOOKUP(user, "lmtp") and, if the
// cluster-wide count is below the limit, sends CONNECT. Returns
// ErrTooManyConcurrent when the limit is reached and
// ErrWardenUnavailable when the warden connection can't be
// established. The id returned is the handle releaseDelivery
// (or releaseAll) will pass to DISCONNECT.
func (c *wardenSessionClient) reserveDelivery(user string) (string, error) {
	if c == nil {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dialLocked(); err != nil {
		return "", err
	}
	// Unlimited path — still register the Connect so `who` sees
	// the LMTP delivery, but skip the LOOKUP call.
	if c.limit > 0 {
		count, err := c.conn.Lookup(user, c.service)
		if err != nil {
			return "", fmt.Errorf("lmtp/warden: lookup: %w", err)
		}
		if count >= c.limit {
			return "", ErrTooManyConcurrent
		}
	}
	id := newWardenSessionID()
	if err := c.conn.Connect(id, user, c.peerIP, c.service); err != nil {
		return "", fmt.Errorf("lmtp/warden: connect: %w", err)
	}
	c.connected = append(c.connected, wardenEntry{id: id, user: user})
	return id, nil
}

// releaseDelivery sends DISCONNECT for one previously-reserved
// id. Silently no-ops on unknown id so the session-level cleanup
// in releaseAll is always safe to call.
func (c *wardenSessionClient) releaseDelivery(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, e := range c.connected {
		if e.id != id {
			continue
		}
		if c.conn != nil {
			if err := c.conn.Disconnect(e.id, e.user, c.peerIP, c.service); err != nil {
				slog.Debug("lmtp/warden: disconnect", "user", e.user, "err", err)
			}
		}
		c.connected = append(c.connected[:i], c.connected[i+1:]...)
		return
	}
}

// releaseAll fires DISCONNECT for every still-outstanding entry
// and closes the connection. Idempotent; safe to call on
// Logout / Reset / Close.
func (c *wardenSessionClient) releaseAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.connected {
		if c.conn != nil {
			if err := c.conn.Disconnect(e.id, e.user, c.peerIP, c.service); err != nil {
				slog.Debug("lmtp/warden: disconnect-all", "user", e.user, "err", err)
			}
		}
	}
	c.connected = nil
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// dialLocked opens the warden connection if it is not already
// open. Caller MUST hold c.mu. A previous dial error is sticky
// for the lifetime of the session — we don't retry an unreachable
// warden mid-LMTP-transaction.
func (c *wardenSessionClient) dialLocked() error {
	if c.conn != nil {
		return nil
	}
	if c.dialErr != nil {
		return c.dialErr
	}
	conn, err := warden.Dial(c.addr, nil, 5*time.Second)
	if err != nil {
		c.dialErr = fmt.Errorf("%w: %v", ErrWardenUnavailable, err)
		return c.dialErr
	}
	c.conn = conn
	return nil
}

// newWardenSessionID returns a hex-encoded random 8-byte handle.
// Unique per Connect within a session — warden keys its
// internal session map on it.
func newWardenSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
