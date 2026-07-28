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

// waitForFile polls for outFile to contain non-empty content, returning it. It gives the
// async best-effort hook a bounded window to run.
func waitForFile(t *testing.T, outFile string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
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
		FlushProgram:         script,
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

	if b := waitForFile(t, out); b != "" {
		t.Fatalf("no hook must run when flush_program is empty, got %q", b)
	}
}

// TestFlushHook_SkippedWithoutOldHost: a move that creates a fresh pin (no prior host)
// attaches no context, so no hook runs — mirroring the reference's old_host_ip guard.
func TestFlushHook_SkippedWithoutOldHost(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hook.log")
	script := writeHookScript(t, out)
	grace := 10 * time.Millisecond
	s := NewWithOptions(Options{
		FlushProgram:         script,
		UserKillConfirmGrace: grace,
		UserKillTimeout:      10 * time.Second,
	})

	user := "fresh@d.test"
	s.moveUser(user, "10.0.0.2:993", nil) // no prior pin → no kill, no attach
	// There is no kill to confirm; drive a sweep anyway to be sure nothing fires.
	confirmUser(t, s, user, grace)

	if b := waitForFile(t, out); b != "" {
		t.Fatalf("a move with no old host must not run the hook, got %q", b)
	}
}
