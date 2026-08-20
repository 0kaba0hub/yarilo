//go:build unix

package director

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeHook writes a hook script that starts a long-lived GRANDCHILD which
// inherits the output pipes, records its pid, and then waits well past the
// bound. This is the field shape: the run was bounded by how long the hook's
// descendant lived, not by the timeout (#1368).
func writeHook(t *testing.T, dir string, holdSeconds int) (script, pidFile string) {
	t.Helper()
	script = filepath.Join(dir, "hook.sh")
	pidFile = filepath.Join(dir, "grandchild.pid")
	body := "#!/bin/sh\n" +
		"echo started\n" +
		"sleep " + strconv.Itoa(holdSeconds) + " &\n" +
		"echo $! > " + pidFile + "\n" +
		"sleep " + strconv.Itoa(holdSeconds) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	return script, pidFile
}

// stillRunning polls for the grandchild's pid to disappear. Polled rather than
// sampled once: the kill is delivered to the group, but the pid stays visible
// until whoever inherits the orphan reaps it, and that is not instant. The
// window is far shorter than the hook's own 20s hold, so a grandchild that
// actually survived the bound still fails the assertion.
func stillRunning(t *testing.T, pidFile string) bool {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("hook never recorded its grandchild: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("grandchild pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// TestFlushHook_BoundHoldsOverTheTree walks the two measurements that separated
// the defects in the field: with the SAME 2s bound, a hook holding 5s took
// 5.0s and one holding 20s took 20.0s — the run scaled with the hook, so the
// bound bounded nothing. Both rows must now finish on the bound instead, and
// neither may leave the grandchild running.
func TestFlushHook_BoundHoldsOverTheTree(t *testing.T) {
	const bound = 1 * time.Second
	for _, hold := range []int{5, 20} {
		t.Run("hook holds "+strconv.Itoa(hold)+"s", func(t *testing.T) {
			dir := t.TempDir()
			script, pidFile := writeHook(t, dir, hold)

			start := time.Now()
			out, timedOut, escaped, err := execFlushProgram(script, bound, "FLUSH", "u@example.com", "1", "10.0.0.1", "10.0.0.2")
			elapsed := time.Since(start)

			if err == nil || !timedOut {
				t.Fatalf("a hook held past the bound must report the timeout: err=%v timedOut=%v out=%q", err, timedOut, out)
			}
			// The bound plus the wait-delay backstop is the whole budget; the
			// hook's own duration must not enter into it.
			if max := bound + flushWaitDelay + time.Second; elapsed > max {
				t.Errorf("run took %v, over the %v budget — the wait scaled with the hook, not the bound", elapsed, max)
			}
			if stillRunning(t, pidFile) {
				t.Error("the hook's grandchild outlived the bound — the group kill did not reach it")
			}
			if escaped {
				t.Errorf("this hook stays in its process group, so the group kill must reach it: escaped=%v", escaped)
			}
		})
	}
}

// TestFlushHook_LongBoundIsNotMistakenForAnEscape: with a bound LONGER than the
// wait delay, an ordinary timeout must still read as a clean kill. The escape
// signal is how long the pipes stayed open AFTER the tree was signalled, not
// how long the run took — measuring the latter marks every slow hook as an
// escape and the report stops meaning anything.
func TestFlushHook_LongBoundIsNotMistakenForAnEscape(t *testing.T) {
	bound := flushWaitDelay + time.Second
	dir := t.TempDir()
	script, pidFile := writeHook(t, dir, 20)

	_, timedOut, escaped, err := execFlushProgram(script, bound, "FLUSH", "u@example.com", "1", "10.0.0.1", "10.0.0.2")
	if err == nil || !timedOut {
		t.Fatalf("err=%v timedOut=%v, want the bound to fire", err, timedOut)
	}
	if escaped {
		t.Error("a hook killed with its group must not be reported as possibly surviving")
	}
	if stillRunning(t, pidFile) {
		t.Error("the grandchild outlived the bound")
	}
}

// TestFlushHook_FastHookIsNotTimedOut: the bound must not fire on a hook that
// finishes, and the output still comes back.
func TestFlushHook_FastHookIsNotTimedOut(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "quick.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho flushed \"$2\"\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	out, timedOut, escaped, err := execFlushProgram(script, 5*time.Second, "FLUSH", "u@example.com", "1", "10.0.0.1", "10.0.0.2")
	if err != nil || timedOut || escaped {
		t.Fatalf("quick hook: err=%v timedOut=%v escaped=%v", err, timedOut, escaped)
	}
	if !strings.Contains(string(out), "flushed u@example.com") {
		t.Errorf("output not captured: %q", out)
	}
}

// helperEscapingHookEnv switches the test binary into hook mode: it stands in
// for an operator script that puts its work into a NEW SESSION, out of reach of
// the process-group kill. Built here rather than shelling out to setsid(1),
// which is not on every platform the tests run on — the case has to be covered
// where it is developed as well as in CI.
const helperEscapingHookEnv = "YARILO_TEST_ESCAPING_HOOK"

func TestHelperEscapingHook(t *testing.T) {
	pidFile := os.Getenv(helperEscapingHookEnv)
	if pidFile == "" {
		t.Skip("helper process; runs only when the hook env is set")
	}
	child := exec.Command("sleep", "20")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The pipes stay inherited: holding them is what used to hold the wait.
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatalf("start escaping child: %v", err)
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
	fmt.Println("started")
	time.Sleep(20 * time.Second)
}

// TestFlushHook_EscapedDescendantStillReleasesTheWait is the other half, and
// the only case the WaitDelay backstop exists for: a hook that calls setsid
// leaves the process group, so the group kill cannot reach it and it keeps the
// output pipes open. Wait must come back on its own delay and SAY the
// descendant may have survived, rather than sitting for as long as it runs.
func TestFlushHook_EscapedDescendantStillReleasesTheWait(t *testing.T) {
	const bound = 1 * time.Second
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	t.Setenv(helperEscapingHookEnv, pidFile)
	script := os.Args[0]
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	start := time.Now()
	_, timedOut, escaped, err := execFlushProgram(script, bound, "-test.run=TestHelperEscapingHook")
	elapsed := time.Since(start)

	if err == nil || !timedOut {
		t.Fatalf("err=%v timedOut=%v, want the bound to fire", err, timedOut)
	}
	if max := bound + flushWaitDelay + time.Second; elapsed > max {
		t.Errorf("run took %v, over the %v budget — the wait sat for the escaped descendant", elapsed, max)
	}
	if !escaped {
		t.Error("a descendant out of the group's reach must be reported, not folded into a plain timeout")
	}
}
