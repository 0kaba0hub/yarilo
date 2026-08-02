// Package quotawarn runs the external action configured for a quota_warning.
// It mirrors the quota_warning execute mechanism and yarilo's own sieve_execute
// runner: a program is located in a fixed bin dir and run best-effort with the
// warning context passed via the environment.
package quotawarn

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/quota"
)

// Runner executes warning programs from a bin dir. A nil *Runner is a no-op, so
// callers can hold one unconditionally.
type Runner struct {
	binDir  string
	timeout time.Duration
}

// New returns a Runner, or nil when no bin dir is configured (warnings then only
// log). timeoutSecs <= 0 defaults to 10s.
func New(binDir string, timeoutSecs int) *Runner {
	if binDir == "" {
		return nil
	}
	t := time.Duration(timeoutSecs) * time.Second
	if t <= 0 {
		t = 10 * time.Second
	}
	return &Runner{binDir: binDir, timeout: t}
}

// Fire evaluates the crossed warnings for a usage transition and runs each one's
// action asynchronously. usageAfter/limits supply the action context. It always
// logs the crossing; the program runs only when a bin dir + Execute are set.
func (r *Runner) Fire(user, home string, warnings []quota.Warning, limits quota.Limits, before, after quota.Usage) {
	for _, w := range quota.MatchWarnings(warnings, limits, before, after) {
		usage, limit := w.ResourceUsageLimit(after, limits)
		slog.Info("quota warning crossed",
			"name", w.Name, "user", user, "resource", w.Resource,
			"threshold", w.Threshold, "percentage", w.Percentage,
			"usage", usage, "limit", limit)
		if r == nil || w.Execute == "" {
			continue
		}
		go r.run(user, home, w, usage, limit)
	}
}

func (r *Runner) run(user, home string, w quota.Warning, usage, limit int64) {
	r.runProgram(w.Execute, user, home, []string{
		"QUOTA_WARNING_NAME=" + w.Name,
		"QUOTA_RESOURCE=" + w.Resource,
		"QUOTA_THRESHOLD=" + w.Threshold,
		"QUOTA_PERCENTAGE=" + strconv.Itoa(w.Percentage),
		"QUOTA_USAGE=" + strconv.FormatInt(usage, 10),
		"QUOTA_LIMIT=" + strconv.FormatInt(limit, 10),
	})
}

// FireOverStatus runs the quota_over_status action when the user's actual
// over-quota state diverges from the flag carried in userdb. It always logs;
// the program runs only with a bin dir + execute set. currentFlag is the stale
// userdb flag, over is the freshly computed real state.
func (r *Runner) FireOverStatus(user, home, execute, currentFlag string, over bool) {
	overStr := "no"
	if over {
		overStr = "yes"
	}
	slog.Info("quota over-status changed", "user", user, "over", over, "flag", currentFlag)
	if r == nil || execute == "" {
		return
	}
	go r.runProgram(execute, user, home, []string{
		"QUOTA_OVER_FLAG=" + currentFlag,
		"QUOTA_OVER=" + overStr,
	})
}

// runProgram runs a bin-dir-confined program with a base env (USER/HOME/HOST)
// plus extra. Best-effort: failures are logged, never surfaced.
func (r *Runner) runProgram(spec, user, home string, extraEnv []string) {
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return
	}
	prog, args := fields[0], fields[1:]
	// Resolve strictly within the bin dir — reject any path separator so a
	// config cannot escape it.
	if strings.ContainsAny(prog, `/\`) {
		slog.Warn("quota execute rejected: program must be a bare name", "program", prog)
		return
	}
	path := filepath.Join(r.binDir, prog)

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append([]string{"USER=" + user, "HOME=" + home}, extraEnv...)
	if host, err := os.Hostname(); err == nil {
		cmd.Env = append(cmd.Env, "HOST="+host)
	}
	if err := cmd.Run(); err != nil {
		slog.Warn("quota execute failed", "program", prog, "err", err)
	}
}
