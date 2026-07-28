package director

import (
	"strings"
	"testing"
	"time"
)

func hashOf(s *Server, user string) uint32 {
	return HashUsername(user, s.opts.usernameHashLowercase())
}

// TestUserKill_LookupHeld: while a user is killing, LOOKUP returns the retryable
// FAIL reason=killing instead of assigning a backend.
func TestUserKill_LookupHeld(t *testing.T) {
	s := NewWithOptions(Options{})
	user := "u@example.com"
	s.startKilling(hashOf(s, user))

	conn := &captureConn{}
	c := &client{conn: conn}
	s.handleLookup(c, []string{"LOOKUP", "7", user, "imap"})

	if got := string(conn.written); !strings.Contains(got, "FAIL\t7\treason=killing") {
		t.Fatalf("a killing user's LOOKUP must be held with reason=killing, got %q", got)
	}
}

// TestUserKill_ReplicatedTTLLocalDeadline: applyKilling takes a DURATION and
// computes the deadline against the local clock (never a wire wall-clock).
func TestUserKill_ReplicatedTTLLocalDeadline(t *testing.T) {
	s := NewWithOptions(Options{})
	hash := hashOf(s, "u@example.com")
	before := time.Now()
	s.applyKilling(hash, 10*time.Second)

	if !s.isKilling(hash) {
		t.Fatal("applyKilling must put the user on hold")
	}
	s.killMu.Lock()
	dl := s.killing[hash].deadline
	s.killMu.Unlock()
	if dl.Before(before.Add(9*time.Second)) || dl.After(time.Now().Add(11*time.Second)) {
		t.Errorf("deadline must be ~local now + ttl, got %v", dl)
	}
}

// TestUserKill_ConfirmClearsAfterGrace: with the session count at zero for the
// confirm grace, the sweep clears the hold.
func TestUserKill_ConfirmClearsAfterGrace(t *testing.T) {
	grace := 50 * time.Millisecond
	s := NewWithOptions(Options{UserKillConfirmGrace: grace, UserKillTimeout: 10 * time.Second})
	user := "u@example.com"
	hash := hashOf(s, user)
	s.startKilling(hash)

	// No sessions for the user -> arming observes zero.
	s.noteSessionClosed(user)
	// Not yet past the grace.
	s.sweepKills(grace)
	if !s.isKilling(hash) {
		t.Fatal("kill must not clear before the confirm grace elapses")
	}
	time.Sleep(grace + 20*time.Millisecond)
	s.sweepKills(grace)
	if s.isKilling(hash) {
		t.Error("kill must clear once the session count has stayed at zero for the grace")
	}
}

// TestUserKill_InflightOpenResetsConfirm is the race guard: a SESSION-OPEN that
// lands mid-kill (a session routed just before the kill) must void a pending
// zero observation, so the hold is not released prematurely.
func TestUserKill_InflightOpenResetsConfirm(t *testing.T) {
	grace := 50 * time.Millisecond
	s := NewWithOptions(Options{UserKillConfirmGrace: grace, UserKillTimeout: 10 * time.Second})
	user := "u@example.com"
	hash := hashOf(s, user)
	s.startKilling(hash)

	s.noteSessionClosed(user) // count 0 -> armed
	s.noteSessionOpened(user) // in-flight open -> disarm

	time.Sleep(grace + 20*time.Millisecond)
	s.sweepKills(grace)
	if !s.isKilling(hash) {
		t.Error("an in-flight SESSION-OPEN must reset the confirm; the hold must not clear")
	}
}

// TestUserKill_TimeoutFallthrough: an unconfirmed kill clears at the hard
// timeout so a stuck holder never locks a user out permanently.
func TestUserKill_TimeoutFallthrough(t *testing.T) {
	s := NewWithOptions(Options{UserKillTimeout: 40 * time.Millisecond, UserKillConfirmGrace: time.Second})
	hash := hashOf(s, "u@example.com")
	s.startKilling(hash)

	// Never confirmed (count never armed). isKilling is lazily false past the
	// deadline, and the sweep removes the record.
	time.Sleep(60 * time.Millisecond)
	if s.isKilling(hash) {
		t.Error("a kill past its hard timeout must no longer hold LOOKUP")
	}
	s.sweepKills(time.Second)
	s.killMu.Lock()
	_, still := s.killing[hash]
	s.killMu.Unlock()
	if still {
		t.Error("the sweep must remove a timed-out kill record")
	}
}
