package imap

import (
	"crypto/tls"
	"log/slog"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/warden"
)

// imapWardenClient is the IMAP-server-wide handle to yarilo-warden
// used to push SELECT events. One TCP connection per process,
// serialised through mu so concurrent Select calls don't
// interleave on the wire.
//
// Lazy: the underlying warden.Conn is opened on the first call
// and reopened on the first transport failure. A nil receiver
// (when WardenAddr is unset) silently no-ops every operation —
// keeps callers free of guard branches.
type imapWardenClient struct {
	addr string
	tls  *tls.Config

	mu   sync.Mutex
	conn *warden.Conn
}

func newImapWardenClient(addr string, tlsCfg *tls.Config) *imapWardenClient {
	if addr == "" {
		return nil
	}
	return &imapWardenClient{addr: addr, tls: tlsCfg}
}

// PushSelect fires SELECT(sessionID, folder) to warden. Empty
// folder means UNSELECT. Best-effort: transport errors are
// logged at Debug and discarded — warden-side state being
// slightly stale never breaks IMAP correctness.
func (c *imapWardenClient) PushSelect(sessionID, folder string) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dialLocked(); err != nil {
		slog.Debug("imap/warden: dial", "err", err)
		return
	}
	if err := c.conn.Select(sessionID, folder); err != nil {
		slog.Debug("imap/warden: select", "sess", sessionID, "folder", folder, "err", err)
		// Drop the conn so the next call redials. Cheap.
		c.conn.Close()
		c.conn = nil
	}
}

// Close releases the underlying connection. Safe on nil.
func (c *imapWardenClient) Close() {
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

// wardenSessionID returns the warden session id the login pod forwarded in the
// YARILO preamble (SESSION=<id>), captured into s.sid by newSession (#808).
// It is the SAME id the login registered the session under in warden, so a
// SELECT push updates the right session. Empty on a direct (non-preamble)
// backend connect, where PushSelect correctly no-ops.
func (s *session) wardenSessionID() string {
	return s.sid
}

// pushWardenSelect fires SELECT(sessionID, folder) to warden so
// the WHO output renders the currently-SELECTed mailbox. Empty
// folder is UNSELECT. Best-effort; no-op when WardenAddr is
// unset or the connection did not carry an XCLIENT session id.
func (s *session) pushWardenSelect(folder string) {
	id := s.wardenSessionID()
	if id == "" {
		return
	}
	s.srv.wardenClient.PushSelect(id, folder)
}

func (c *imapWardenClient) dialLocked() error {
	if c.conn != nil {
		return nil
	}
	conn, err := warden.Dial(c.addr, c.tls, 5*time.Second)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}
