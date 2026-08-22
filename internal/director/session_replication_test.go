package director

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// TestApplyRemoteSession_FeedsCounts guards #804: a session gossiped from
// another director is counted locally (so least_sessions sees the cluster view)
// and removed on the matching SESSION-CLOSE.
func TestApplyRemoteSession_FeedsCounts(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})

	s.applyRemoteSessionOpen([]string{"sid1", "u@d.test", "10.0.0.5", "imap"}, "10.0.0.99@run1")
	total, byProto := s.sessionCounts()
	if total["10.0.0.5"] != 1 || byProto["10.0.0.5"]["imap"] != 1 {
		t.Fatalf("remote session must be counted, got total=%v byProto=%v", total, byProto)
	}
	// The record is remote (cl == nil).
	s.sessRecMu.RLock()
	rec := s.sessById["sid1"]
	s.sessRecMu.RUnlock()
	if rec == nil || rec.cl != nil {
		t.Fatalf("remote record must have cl==nil, got %+v", rec)
	}

	s.applyRemoteSessionClose("sid1")
	if total, _ := s.sessionCounts(); total["10.0.0.5"] != 0 {
		t.Fatalf("remote SESSION-CLOSE must drop the count, got %v", total)
	}
}

// TestApplyRemoteSessionOpen_DoesNotClobberLocal: a stray remote SESSION-OPEN
// for an id we own locally must not overwrite our (kickable) local record.
func TestApplyRemoteSessionOpen_DoesNotClobberLocal(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	local := &client{} // non-nil owning conn marker
	s.sessRecMu.Lock()
	s.sessById["sid1"] = &sessionRec{id: "sid1", backend: "10.0.0.1", proto: "imap", cl: local}
	s.sessByBE["10.0.0.1"] = map[string]bool{"sid1": true}
	s.sessRecMu.Unlock()

	s.applyRemoteSessionOpen([]string{"sid1", "u", "10.0.0.9", "imap"}, "10.0.0.99@run1")

	s.sessRecMu.RLock()
	rec := s.sessById["sid1"]
	s.sessRecMu.RUnlock()
	if rec.cl != local || rec.backend != "10.0.0.1" {
		t.Fatalf("local record must survive a remote SESSION-OPEN, got %+v", rec)
	}
}

// TestKickSessionsForBackend_SkipsRemote guards #804: kick must not deref a
// remote record's nil conn, and must still remove remote records from the view.
func TestKickSessionsForBackend_SkipsRemote(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	s.ring.AddBackend(&ring.Backend{IP: "10.0.0.5", Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	// One remote session (cl == nil) on the backend.
	s.applyRemoteSessionOpen([]string{"sid1", "u@d.test", "10.0.0.5", "imap"}, "10.0.0.99@run1")

	// Must not panic on the nil conn, and must clear the registry for the backend.
	s.kickSessionsForBackend("10.0.0.5")

	if total, _ := s.sessionCounts(); total["10.0.0.5"] != 0 {
		t.Fatalf("kick must remove the remote records too, got %v", total)
	}
}
