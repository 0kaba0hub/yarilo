package imap

import (
	"crypto/tls"
	"log/slog"
	"sync"
	"time"

	"github.com/0kaba0hub/yarilo/internal/anvil"
)

// imapAnvilClient is the IMAP-server-wide handle to yarilo-anvil
// used to push SELECT events. One TCP connection per process,
// serialised through mu so concurrent Select calls don't
// interleave on the wire.
//
// Lazy: the underlying anvil.Conn is opened on the first call
// and reopened on the first transport failure. A nil receiver
// (when AnvilAddr is unset) silently no-ops every operation —
// keeps callers free of guard branches.
type imapAnvilClient struct {
	addr string
	tls  *tls.Config

	mu   sync.Mutex
	conn *anvil.Conn
}

func newImapAnvilClient(addr string, tlsCfg *tls.Config) *imapAnvilClient {
	if addr == "" {
		return nil
	}
	return &imapAnvilClient{addr: addr, tls: tlsCfg}
}

// PushSelect fires SELECT(sessionID, folder) to anvil. Empty
// folder means UNSELECT. Best-effort: transport errors are
// logged at Debug and discarded — anvil-side state being
// slightly stale never breaks IMAP correctness.
func (c *imapAnvilClient) PushSelect(sessionID, folder string) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dialLocked(); err != nil {
		slog.Debug("imap/anvil: dial", "err", err)
		return
	}
	if err := c.conn.Select(sessionID, folder); err != nil {
		slog.Debug("imap/anvil: select", "sess", sessionID, "folder", folder, "err", err)
		// Drop the conn so the next call redials. Cheap.
		c.conn.Close()
		c.conn = nil
	}
}

// Close releases the underlying connection. Safe on nil.
func (c *imapAnvilClient) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// anvilSessionID returns the anvil session id the login pod forwarded in the
// YARILO preamble (SESSION=<id>), captured into s.sid by newSession (#808).
// It is the SAME id the login registered the session under in anvil, so a
// SELECT push updates the right session. Empty on a direct (non-preamble)
// backend connect, where PushSelect correctly no-ops.
func (s *session) anvilSessionID() string {
	return s.sid
}

// pushAnvilSelect fires SELECT(sessionID, folder) to anvil so
// the WHO output renders the currently-SELECTed mailbox. Empty
// folder is UNSELECT. Best-effort; no-op when AnvilAddr is
// unset or the connection did not carry an XCLIENT session id.
func (s *session) pushAnvilSelect(folder string) {
	id := s.anvilSessionID()
	if id == "" {
		return
	}
	s.srv.anvilClient.PushSelect(id, folder)
}

func (c *imapAnvilClient) dialLocked() error {
	if c.conn != nil {
		return nil
	}
	conn, err := anvil.Dial(c.addr, c.tls, 5*time.Second)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}
