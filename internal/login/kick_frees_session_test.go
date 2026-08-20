package login

import (
	"net"
	"testing"
	"time"
)

// proxiedPair wires a session the way a live one is wired: two connections
// bridged by biProxy. The client end is held open and SILENT, which is the
// field shape — a kick has to end the session on its own, not wait for the
// client to do something. Returns the session and a channel closed when the
// proxy returns (the moment the session record is dropped and SESSION-CLOSE is
// sent in the real path).
func proxiedPair(t *testing.T, id, user string) (*liveSession, net.Conn, <-chan struct{}) {
	t.Helper()
	clientEnd, clientSide := net.Pipe()
	backendSide, backendEnd := net.Pipe()
	t.Cleanup(func() { clientEnd.Close(); clientSide.Close(); backendSide.Close(); backendEnd.Close() })

	done := make(chan struct{})
	go func() {
		biProxy(clientSide, clientSide, backendSide, backendSide)
		close(done)
	}()
	return &liveSession{id: id, user: user, backendConn: backendSide, clientConn: clientSide}, clientEnd, done
}

// TestKickUser_FreesTheSessionWhileTheClientIsSilent is #1366 in its field
// form. Closing the backend leg alone ends one of biProxy's two copies; the
// other sits on the client, which here says nothing and does not leave. The
// proxy must still return -- otherwise the session record survives, no
// SESSION-CLOSE reaches the director, the kill window burns out and the flush
// hook never runs. Measured in the field as 55s and 81s: both were the test
// client leaving, not the server acting.
func TestKickUser_FreesTheSessionWhileTheClientIsSilent(t *testing.T) {
	s := &Server{opts: Options{Protocol: ProtocolIMAP}, sessions: make(map[string][]*liveSession)}
	sess, client, done := proxiedPair(t, "s1", "u@example.com")
	s.sessions["u@example.com"] = []*liveSession{sess}
	_ = client // held open on purpose, never read from, never closed

	s.kickUser("u@example.com")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the proxy is still running after the kick: the session record is never freed")
	}
}

// TestKickSession_FreesTheSessionWhileTheClientIsSilent: the warden path kicks
// one session by id and had the same half-close defect.
func TestKickSession_FreesTheSessionWhileTheClientIsSilent(t *testing.T) {
	s := &Server{opts: Options{Protocol: ProtocolIMAP}, sessions: make(map[string][]*liveSession)}
	sess, client, done := proxiedPair(t, "s2", "u@example.com")
	s.sessions["u@example.com"] = []*liveSession{sess}
	_ = client

	if !s.kickSession("s2") {
		t.Fatal("kickSession returned false for a live id")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the proxy is still running after the kick by session id")
	}
}
