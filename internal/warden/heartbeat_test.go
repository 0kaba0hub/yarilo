package warden

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSweepStaleSessions covers the core sweeper invariant: a
// session whose lastSeen is older than the configured TTL gets
// dropped from the session map AND its connection-limit slot
// returns to the pool, so the next CONNECT from the same
// (user, ip) succeeds.
func TestSweepStaleSessions(t *testing.T) {
	s := NewServer(1, WithSessionTTL(100*time.Millisecond))
	mb := s.state.(*memoryBackend) // white-box: the memory backend holds the maps
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mb.sessions["sess-stale"] = &SessionInfo{
		ID:          "sess-stale",
		User:        "alice@example.com",
		IP:          "10.0.0.1",
		Service:     "imap",
		ConnectedAt: now.Add(-time.Hour),
		lastSeen:    now.Add(-time.Hour), // way past TTL
	}
	mb.sessions["sess-fresh"] = &SessionInfo{
		ID:          "sess-fresh",
		User:        "bob@example.com",
		IP:          "10.0.0.2",
		Service:     "imap",
		ConnectedAt: now,
		lastSeen:    now.Add(-50 * time.Millisecond), // inside TTL
	}
	// Tell the limiter both slots are taken so reap can release.
	if !mb.limiter.Acquire("alice@example.com", "10.0.0.1") {
		t.Fatal("alice acquire")
	}
	if !mb.limiter.Acquire("bob@example.com", "10.0.0.2") {
		t.Fatal("bob acquire")
	}

	mb.Maintain(now)

	if _, ok := mb.sessions["sess-stale"]; ok {
		t.Error("stale session not reaped")
	}
	if _, ok := mb.sessions["sess-fresh"]; !ok {
		t.Error("fresh session reaped prematurely")
	}
	// Alice's slot should be released — a fresh acquire passes.
	if !mb.limiter.Acquire("alice@example.com", "10.0.0.1") {
		t.Error("alice slot not released by sweep")
	}
	// Bob's slot is still held — a second acquire fails.
	if mb.limiter.Acquire("bob@example.com", "10.0.0.2") {
		t.Error("bob slot leaked into pool — fresh session reaped")
	}
}

// TestHandleHeartbeat_KnownSession bumps lastSeen so a stale
// session becomes fresh again under the sweep window.
func TestHandleHeartbeat_KnownSession(t *testing.T) {
	s := NewServer(0, WithSessionTTL(100*time.Millisecond))
	mb := s.state.(*memoryBackend)
	old := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mb.sessions["sess-1"] = &SessionInfo{
		ID:          "sess-1",
		User:        "alice@example.com",
		IP:          "10.0.0.1",
		Service:     "imap",
		ConnectedAt: old,
		lastSeen:    old,
	}

	// net.Pipe() gives us a real net.Conn pair; handleHeartbeat
	// only needs the write side. Drain the read side in the
	// background so the write doesn't block.
	cliConn, srvConn := net.Pipe()
	defer cliConn.Close()
	defer srvConn.Close()
	go io.Copy(io.Discard, cliConn) //nolint:errcheck

	s.handleHeartbeat(srvConn, []string{"HEARTBEAT", "sess-1"})

	if got := mb.sessions["sess-1"].lastSeen; !got.After(old) {
		t.Errorf("lastSeen not bumped: still %v", got)
	}
}

// TestHandleHeartbeat_UnknownReturnsReason — the protocol
// contract for callers that need to re-CONNECT after a reap.
func TestHandleHeartbeat_UnknownReturnsReason(t *testing.T) {
	s := NewServer(0)
	cliConn, srvConn := net.Pipe()
	defer cliConn.Close()
	defer srvConn.Close()

	done := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(cliConn)
		if sc.Scan() {
			done <- sc.Text()
			return
		}
		done <- ""
	}()

	s.handleHeartbeat(srvConn, []string{"HEARTBEAT", "ghost"})
	line := <-done
	if !strings.Contains(line, "OK\tghost\treason=unknown") {
		t.Errorf("missing reason=unknown for ghost session: %q", line)
	}
}
