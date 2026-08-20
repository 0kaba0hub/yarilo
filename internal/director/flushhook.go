package director

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"
)

// flushHookTimeout bounds one flush-hook run (#848). The reference uses a 10s connect
// timeout for its flush socket; we bound the whole external program the same way so a
// wedged hook can never accumulate goroutines/processes without limit.
const defaultFlushProgramTimeout = 10 * time.Second

// flushWaitDelay is how long Wait may still sit on the output pipes after the
// hook's process group has been killed. It is a backstop, not a second bound:
// it only matters for a descendant the group kill could not reach.
const flushWaitDelay = 2 * time.Second

// flushProgramTimeout is the operator's bound, or the default when unset.
func (s *Server) flushProgramTimeout() time.Duration {
	if s.opts.FlushProgramTimeout > 0 {
		return s.opts.FlushProgramTimeout
	}
	return defaultFlushProgramTimeout
}

// runFlushHook invokes the configured per-user flush program asynchronously once a
// relocation has been confirmed ring-wide (#848). It is strictly best-effort: the
// routing change already happened and replicated, so a slow or failing hook is logged
// and discarded, never blocking the ring/LOOKUP path or failing the move. Only the
// director that originated the move reaches here (the flush context is local).
//
// The program is called as:
//
//	flush_program FLUSH <username> <username_hash> <old_backend> <new_backend>
//
// new_backend is empty when the user had nowhere left to land (they were kicked, not
// moved) — the hook can still clean up the old backend.
func (s *Server) runFlushHook(hash uint32, fc flushCtx) {
	prog := s.opts.FlushProgram
	if prog == "" {
		return
	}
	go func() {
		bound := s.flushProgramTimeout()
		out, timedOut, escaped, err := execFlushProgram(prog, bound, "FLUSH", fc.user,
			strconv.FormatUint(uint64(hash), 10), fc.oldBackend, fc.newBackend)
		switch {
		case timedOut:
			// Reported off the deadline rather than the exit status: a killed
			// hook reports "signal: killed" whoever killed it, and an operator
			// reading that cannot tell the bound from a crash.
			slog.Warn("director: flush hook exceeded its timeout and was killed",
				"program", prog, "user", fc.user, "hash", hash,
				"timeout", bound, "descendant_may_have_survived", escaped,
				"output", string(out))
		case err != nil:
			slog.Warn("director: flush hook failed (best-effort, move already committed)",
				"program", prog, "user", fc.user, "hash", hash,
				"old_backend", fc.oldBackend, "new_backend", fc.newBackend,
				"err", err, "output", string(out))
		default:
			slog.Debug("director: flush hook ran", "user", fc.user, "hash", hash,
				"old_backend", fc.oldBackend, "new_backend", fc.newBackend)
		}
	}()
}

// execFlushProgram runs one hook under a hard bound and reports what the bound
// did. Separated from the logging so the property that matters -- it returns
// within the bound, and takes the hook's descendants with it -- is asserted
// where it is computed.
//
// timedOut says the bound, not the hook, ended the run. escaped says Wait came
// back on its own delay with output pipes still held, which means something in
// the tree was beyond reach of the group kill (a hook that called setsid) and
// may still be running. That is the one case where the bound stops the waiting
// without stopping the work, so it is reported rather than folded into "failed".
func execFlushProgram(prog string, bound time.Duration, args ...string) (out []byte, timedOut, escaped bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, args...)
	// The bound has to hold over the whole tree, not the program alone.
	// CombinedOutput waits for the output pipes to close and a grandchild
	// inherits them, so killing the direct child left the wait sitting for as
	// long as the grandchild ran: the bound measured nothing, and the
	// grandchild outlived it either way (#1368).
	isolateProcessGroup(cmd)
	var killedAt atomic.Int64
	cmd.Cancel = func() error {
		killedAt.Store(time.Now().UnixNano())
		return killProcessTree(cmd)
	}
	cmd.WaitDelay = flushWaitDelay
	out, err = cmd.CombinedOutput()
	if err == nil || ctx.Err() == nil {
		return out, false, false, err
	}
	// Read off the clock rather than the error: when the delay releases a wait
	// the process has already been signalled, so Wait reports "signal: killed"
	// and ErrWaitDelay never surfaces. Measured from the kill, not from the
	// start -- the question is how long the pipes stayed open AFTER the tree was
	// signalled, and a bound longer than the delay would otherwise mark every
	// timeout as an escape.
	killed := killedAt.Load()
	escaped = killed != 0 && time.Since(time.Unix(0, killed)) >= flushWaitDelay
	return out, true, escaped, err
}
