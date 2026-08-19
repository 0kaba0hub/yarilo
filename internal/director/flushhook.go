package director

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"time"
)

// flushHookTimeout bounds one flush-hook run (#848). The reference uses a 10s connect
// timeout for its flush socket; we bound the whole external program the same way so a
// wedged hook can never accumulate goroutines/processes without limit.
const defaultFlushProgramTimeout = 10 * time.Second

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
		ctx, cancel := context.WithTimeout(context.Background(), s.flushProgramTimeout())
		defer cancel()
		cmd := exec.CommandContext(ctx, prog, "FLUSH", fc.user,
			strconv.FormatUint(uint64(hash), 10), fc.oldBackend, fc.newBackend)
		out, err := cmd.CombinedOutput()
		if err != nil {
			slog.Warn("director: flush hook failed (best-effort, move already committed)",
				"program", prog, "user", fc.user, "hash", hash,
				"old_backend", fc.oldBackend, "new_backend", fc.newBackend,
				"err", err, "output", string(out))
			return
		}
		slog.Debug("director: flush hook ran", "user", fc.user, "hash", hash,
			"old_backend", fc.oldBackend, "new_backend", fc.newBackend)
	}()
}
