package director

import (
	"testing"
	"time"
)

// TestTouch_ExtendsTTLPreservesStamp: Touch bumps ExpiresAt without changing the
// assignment stamp or host (#708 PR-B) — a refresh, not a re-assignment.
func TestTouch_ExtendsTTLPreservesStamp(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	h := s.userDir.Set("u@d.test", "10.0.0.1:10143", false)
	seq, by, _ := s.userDir.LastAssign(h)

	// Force it expired, then confirm Get drops it.
	s.userDir.byHash[h].ExpiresAt = time.Now().Add(-time.Hour)
	if s.userDir.GetByHash(h) != nil {
		t.Fatal("precondition: entry should read as expired")
	}

	if !s.userDir.Touch("u@d.test") {
		t.Fatal("Touch must find and refresh the entry")
	}
	e := s.userDir.GetByHash(h)
	if e == nil {
		t.Fatal("Touch must revive the pin while a session justifies it")
	}
	if e.Host != "10.0.0.1:10143" {
		t.Fatalf("Touch must not change host, got %q", e.Host)
	}
	if seq2, by2, _ := s.userDir.LastAssign(h); seq2 != seq || by2 != by {
		t.Fatalf("Touch must not change the assignment stamp: (%d,%q) -> (%d,%q)", seq, by, seq2, by2)
	}
	if s.userDir.Touch("nobody@d.test") {
		t.Fatal("Touch on a missing user must return false")
	}
}

// TestRefreshPinnedSessions_OnlyLiveUsers guards #708 PR-B: the pin of a user
// with a live session is kept fresh; a user without one lapses (expires).
func TestRefreshPinnedSessions_OnlyLiveUsers(t *testing.T) {
	s := NewWithOptions(Options{AntiEntropyInterval: -1})
	hLive := s.userDir.Set("live@d.test", "10.0.0.1:10143", false)
	hIdle := s.userDir.Set("idle@d.test", "10.0.0.2:10143", false)

	// Both about to expire.
	s.userDir.byHash[hLive].ExpiresAt = time.Now().Add(-time.Hour)
	s.userDir.byHash[hIdle].ExpiresAt = time.Now().Add(-time.Hour)

	// Only 'live' has an active session.
	s.sessRecMu.Lock()
	s.sessById["s1"] = &sessionRec{id: "s1", user: "live@d.test", backend: "10.0.0.1", proto: "imap"}
	s.sessByBE["10.0.0.1"] = map[string]bool{"s1": true}
	s.sessRecMu.Unlock()

	s.refreshPinnedSessions()

	if s.userDir.GetByHash(hLive) == nil {
		t.Fatal("a user with a live session must keep its pin (refreshed)")
	}
	if s.userDir.GetByHash(hIdle) != nil {
		t.Fatal("an idle user's pin must lapse (not refreshed) → falls back to the ring hash")
	}
}
