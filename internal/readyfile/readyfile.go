// Package readyfile implements the co-located backend pod's readiness signal
// (#788): each protocol container periodically touches a file in a shared
// directory ONLY while it is actually ready, and the yarilo-backend-reg sidecar
// gates its director heartbeat on all those files being fresh. mtime-staleness
// is a passive proof of life — a wedged data path simply stops touching, so the
// file goes stale and the pod is expired ring-wide, rather than an active HTTP
// probe that a stalled data path can still answer 200.
package readyfile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Touch periodically updates the mtime of dir/proto — but ONLY while ready()
// returns true. The touch MUST be tied to the real readiness condition (the
// listener accepting and not wedged), never to "the process is alive": an
// unconditional toucher would reduce mtime-staleness to the same "live
// goroutine != live data path" hole the file mechanism exists to close.
//
// dir == "" disables the writer (non-co-located / standalone runs). Blocks
// until ctx is done.
func Touch(ctx context.Context, dir, proto string, interval time.Duration, ready func() bool) {
	if dir == "" {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("readyfile: cannot create dir, readiness signal disabled", "dir", dir, "err", err)
		return
	}
	path := filepath.Join(dir, proto)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ready != nil && !ready() {
				// Not ready → do not touch. The file goes stale and the
				// sidecar stops heartbeating → the director expires this pod.
				continue
			}
			touch(path)
		}
	}
}

func touch(path string) {
	// Create if missing, then bump mtime to now.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("readyfile: touch open failed", "path", path, "err", err)
		return
	}
	_ = f.Close()
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		slog.Warn("readyfile: chtimes failed", "path", path, "err", err)
	}
}

// AllFresh reports whether every expected protocol's readiness file exists in
// dir and was touched within staleAfter. A missing or stale file means "not
// ready" — the sidecar then withholds its heartbeat. The returned string names
// the first failing protocol (for logging); it is "" when all are fresh.
func AllFresh(dir string, protos []string, staleAfter time.Duration) (bool, string) {
	if staleAfter <= 0 {
		staleAfter = 15 * time.Second
	}
	for _, p := range protos {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			return false, fmt.Sprintf("%s: missing", p)
		}
		if age := time.Since(fi.ModTime()); age > staleAfter {
			return false, fmt.Sprintf("%s: stale (%s > %s)", p, age.Round(time.Millisecond), staleAfter)
		}
	}
	return true, ""
}
