package director

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeHookScript creates an executable that appends its argv to outFile, one run per
// line, and returns its path.
func writeHookScript(t *testing.T, outFile string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "flush.sh")
	body := "#!/bin/sh\necho \"$1 $2 $3 $4 $5\" >> " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return script
}

// waitForFile polls until outFile has content, and is deliberately patient.
//
// The hook is an external process: the wait covers a fork, an exec and a write
// on a machine that may be running every other package's tests at the same
// time. A generous deadline costs nothing when the hook is on time -- the loop
// returns on its first iteration -- and the two-second one it replaces failed
// exactly at its limit under a full-tree run while passing alone every time
// (#1475).
//
// Patience also sharpens the failure: if this now fails, the hook did not run
// in half a minute, which is a statement about the hook rather than about the
// machine.
func waitForFile(t *testing.T, outFile string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

// expectNoFile is the opposite assertion and needs the opposite deadline: it
// waits a fixed window to give a hook that should not run the chance to run
// anyway, then declares it did not.
//
// Waiting longer here buys nothing -- a hook that has not fired in two seconds
// is not firing -- and would only make the suite slower, which is why the two
// waits are separate rather than one number serving both.
func expectNoFile(t *testing.T, outFile string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			t.Fatalf("a hook ran that must not have: %q", strings.TrimSpace(string(b)))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// confirmUser drives a user's kill to the confirmed state (no active sessions → armed at
// zero → swept past the grace), firing any attached flush hook.
func confirmUser(t *testing.T, s *Server, user string, grace time.Duration) {
	t.Helper()
	s.noteSessionClosed(user)
	time.Sleep(grace + 20*time.Millisecond)
	s.sweepKills(grace)
}

// TestFlushHook_RunsOnConfirmedMove: an admin move fires the hook after the kill
// confirms, passing FLUSH <user> <hash> <old> <new>.
func TestFlushHook_RunsOnConfirmedMove(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hook.log")
	script := writeHookScript(t, out)
	grace := 10 * time.Millisecond
	s := NewWithOptions(Options{
		FlushProgram: script,
		// See the note in idlekill_test.go: these assert that the hook runs,
		// not how fast a shell script starts on a loaded machine (#1352).
		FlushProgramTimeout:  2 * time.Minute,
		UserKillConfirmGrace: grace,
		UserKillTimeout:      10 * time.Second,
	})

	user := "u@d.test"
	s.userDir.Set(user, "10.0.0.1:993", false) // old pin
	s.moveUser(user, "10.0.0.2:993", nil)      // relocate → startKilling + attachFlush
	confirmUser(t, s, user, grace)

	got := strings.TrimSpace(waitForFile(t, out))
	want := fmt.Sprintf("FLUSH %s %d 10.0.0.1 10.0.0.2", user, HashUsername(user, s.hf))
	if got != want {
		t.Fatalf("hook argv = %q, want %q", got, want)
	}
}

// TestFlushHook_DisabledByDefault: no flush_program → no hook, even on a confirmed move.
func TestFlushHook_DisabledByDefault(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hook.log")
	grace := 10 * time.Millisecond
	s := NewWithOptions(Options{UserKillConfirmGrace: grace, UserKillTimeout: 10 * time.Second})

	user := "u@d.test"
	s.userDir.Set(user, "10.0.0.1:993", false)
	s.moveUser(user, "10.0.0.2:993", nil)
	confirmUser(t, s, user, grace)

	expectNoFile(t, out)
}

// TestFlushHook_SkippedWithoutOldHost: a move that creates a fresh pin (no prior host)
// attaches no context, so no hook runs — mirroring the reference's old_host_ip guard.
func TestFlushHook_SkippedWithoutOldHost(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hook.log")
	script := writeHookScript(t, out)
	grace := 10 * time.Millisecond
	s := NewWithOptions(Options{
		FlushProgram: script,
		// See the note in idlekill_test.go: these assert that the hook runs,
		// not how fast a shell script starts on a loaded machine (#1352).
		FlushProgramTimeout:  2 * time.Minute,
		UserKillConfirmGrace: grace,
		UserKillTimeout:      10 * time.Second,
	})

	user := "fresh@d.test"
	s.moveUser(user, "10.0.0.2:993", nil) // no prior pin → no kill, no attach
	// There is no kill to confirm; drive a sweep anyway to be sure nothing fires.
	confirmUser(t, s, user, grace)

	expectNoFile(t, out)
}

// The bound is the operator's, and the default is what applies when they say
// nothing. Without the first row a zero value would mean "no timeout at all"
// after some future refactor, and a hung hook would leak a process per move;
// without the second the knob could be ignored and every test above would
// still pass, because they only need it to be generous (#1352).
func TestFlushProgramTimeoutResolution(t *testing.T) {
	tests := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"unset falls back to the default", 0, defaultFlushProgramTimeout},
		{"negative falls back too", -time.Second, defaultFlushProgramTimeout},
		{"an operator's value is used", 45 * time.Second, 45 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewWithOptions(Options{FlushProgramTimeout: tc.set})
			if got := s.flushProgramTimeout(); got != tc.want {
				t.Errorf("timeout = %s, want %s", got, tc.want)
			}
		})
	}
}
