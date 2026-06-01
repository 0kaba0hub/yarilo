package lmtp

import (
	"net"
	"testing"
	"time"
)

// TestKickSession_ClosesMtaConn verifies that kickSession finds
// a registered session and closes its mtaConn. The "other side"
// of the pipe detects the close — that is the proof go-smtp's
// session machinery would unwind cleanly in production.
func TestKickSession_ClosesMtaConn(t *testing.T) {
	s := &Server{sessions: make(map[string]*session)}
	mtaEnd, srvEnd := net.Pipe()
	defer mtaEnd.Close()
	sess := &session{srv: s, peerIP: "10.0.0.1", mtaConn: srvEnd}
	s.registerSession("rcpt-1", sess)
	s.registerSession("rcpt-2", sess) // same conn, multiple RCPTs

	if !s.kickSession("rcpt-1") {
		t.Fatal("kickSession returned false for live id")
	}

	mtaEnd.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := mtaEnd.Read(buf); err == nil {
		t.Error("mtaConn read returned no error after kick — conn still open")
	}

	// Second id resolves to the same (now-dead) conn — kick is a
	// no-op-after-close, not an error.
	if !s.kickSession("rcpt-2") {
		t.Error("second id should still resolve before unregister")
	}
}

func TestKickSession_NoMatchIsNoop(t *testing.T) {
	s := &Server{sessions: make(map[string]*session)}
	if s.kickSession("ghost") {
		t.Fatal("kickSession returned true for missing id")
	}
}

func TestUnregisterSessionIDs_DropsKeys(t *testing.T) {
	s := &Server{sessions: make(map[string]*session)}
	sess := &session{srv: s}
	s.registerSession("a", sess)
	s.registerSession("b", sess)
	s.registerSession("c", sess)
	s.unregisterSessionIDs(map[string]string{
		"rcpt-1": "a",
		"rcpt-2": "b",
	})
	if _, ok := s.sessions["a"]; ok {
		t.Error("a not dropped")
	}
	if _, ok := s.sessions["b"]; ok {
		t.Error("b not dropped")
	}
	if _, ok := s.sessions["c"]; !ok {
		t.Error("c dropped — unrelated id")
	}
}
