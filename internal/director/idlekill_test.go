package director

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStartKilling_ArmsConfirmForIdleUser proves the #870 fix: a kill of a user with
// zero active sessions arms the confirm at kill-start, so it confirms after the grace
// instead of only ever falling through to the hard timeout.
func TestStartKilling_ArmsConfirmForIdleUser(t *testing.T) {
	s := NewWithOptions(Options{UserKillConfirmGrace: 10 * time.Millisecond, UserKillTimeout: 30 * time.Second})
	hash := HashUsername("idle@d.test", s.hf)

	s.startKilling(hash)

	s.killMu.Lock()
	armed := !s.killing[hash].zeroSince.IsZero()
	s.killMu.Unlock()
	if !armed {
		t.Fatal("startKilling must arm zeroSince for a user with zero sessions (#870)")
	}

	time.Sleep(20 * time.Millisecond)
	s.sweepKills(10 * time.Millisecond)
	if s.isKilling(hash) {
		t.Error("idle-user kill must confirm after the grace, not wait for the timeout")
	}
}

// TestStartKilling_NotArmedWithActiveSession is the guard: a user WITH a live session
// must NOT arm at kill-start — it still waits for the session to drain (transition to
// zero) before confirming, exactly as before.
func TestStartKilling_NotArmedWithActiveSession(t *testing.T) {
	s := NewWithOptions(Options{UserKillConfirmGrace: 10 * time.Millisecond, UserKillTimeout: 30 * time.Second})
	user := "busy@d.test"
	openSession(t, s, "s1", user, "10.0.0.1") // one active session
	hash := HashUsername(user, s.hf)

	s.startKilling(hash)

	s.killMu.Lock()
	armed := !s.killing[hash].zeroSince.IsZero()
	s.killMu.Unlock()
	if armed {
		t.Fatal("a user with an active session must NOT arm the confirm at kill-start")
	}
}

// TestFlushHook_RunsForIdleUserMove is the end-to-end #870 gate: an admin move of an
// idle (unconnected) user runs the flush hook after the grace — WITHOUT any SESSION-CLOSE
// to arm the confirm (the sandbox repro), which pre-fix would have skipped the hook and
// waited out the 15s timeout.
func TestFlushHook_RunsForIdleUserMove(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hook.log")
	script := writeHookScript(t, out)
	grace := 10 * time.Millisecond
	s := NewWithOptions(Options{
		FlushProgram:         script,
		UserKillConfirmGrace: grace,
		UserKillTimeout:      30 * time.Second, // long: a timeout-driven exit would NOT run the hook
		// Generous on purpose: these tests assert that the hook RUNS, not
		// how fast a shell script starts. With the production default of ten
		// seconds they failed on a loaded machine -- the script was killed and
		// the best-effort path logged it, exactly as designed -- so the test
		// was stricter than the contract it checks (#1352).
		FlushProgramTimeout: 2 * time.Minute,
	})

	user := "idle@d.test"
	s.userDir.Set(user, "10.0.0.1:993", false)
	s.moveUser(user, "10.0.0.2:993", nil)

	// No noteSessionClosed — the user had no sessions. Only sweep past the grace.
	time.Sleep(grace + 20*time.Millisecond)
	s.sweepKills(grace)

	got := strings.TrimSpace(waitForFile(t, out))
	if !strings.HasPrefix(got, "FLUSH "+user+" ") {
		t.Fatalf("flush hook must run for an idle-user move without waiting the timeout; got %q", got)
	}
}
