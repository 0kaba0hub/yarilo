package readyfile

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeAt(t *testing.T, dir, proto string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, proto)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestAllFresh(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		present    map[string]time.Time // proto → mtime
		protos     []string
		staleAfter time.Duration
		want       bool
	}{
		{
			name:       "all present and fresh",
			present:    map[string]time.Time{"imap": now, "lmtp": now},
			protos:     []string{"imap", "lmtp"},
			staleAfter: 15 * time.Second,
			want:       true,
		},
		{
			name:       "one missing",
			present:    map[string]time.Time{"imap": now},
			protos:     []string{"imap", "lmtp"},
			staleAfter: 15 * time.Second,
			want:       false,
		},
		{
			name:       "one stale",
			present:    map[string]time.Time{"imap": now, "lmtp": now.Add(-30 * time.Second)},
			protos:     []string{"imap", "lmtp"},
			staleAfter: 15 * time.Second,
			want:       false,
		},
		{
			name:       "empty protocol set is trivially fresh",
			present:    map[string]time.Time{},
			protos:     nil,
			staleAfter: 15 * time.Second,
			want:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for p, m := range tt.present {
				writeAt(t, dir, p, m)
			}
			got, reason := AllFresh(dir, tt.protos, tt.staleAfter)
			if got != tt.want {
				t.Fatalf("AllFresh = %v (%q), want %v", got, reason, tt.want)
			}
			if !got && reason == "" {
				t.Error("failing AllFresh must return a non-empty reason")
			}
		})
	}
}

// TestTouch_OnlyWhenReady guards requirement 1: the toucher must touch ONLY
// while ready() is true, so an unready container's file goes stale.
func TestTouch_OnlyWhenReady(t *testing.T) {
	dir := t.TempDir()
	ready := &atomicBool{}
	ready.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Touch(ctx, dir, "imap", 10*time.Millisecond, ready.Load)

	// While ready: the file appears and stays fresh.
	path := filepath.Join(dir, "imap")
	waitFile(t, path, time.Second)
	if fresh, _ := AllFresh(dir, []string{"imap"}, 100*time.Millisecond); !fresh {
		t.Fatal("file should be fresh while ready")
	}

	// Flip to not-ready: touching stops, mtime freezes, file goes stale.
	ready.Store(false)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fresh, _ := AllFresh(dir, []string{"imap"}, 80*time.Millisecond); !fresh {
			return // went stale as required
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("file never went stale after ready flipped to false — touch is not gated on readiness")
}

// TestTouch_EmptyDirIsNoop: dir=="" returns immediately (standalone runs).
func TestTouch_EmptyDirIsNoop(t *testing.T) {
	done := make(chan struct{})
	go func() {
		Touch(context.Background(), "", "imap", time.Millisecond, func() bool { return true })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Touch with empty dir should return immediately")
	}
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared", path)
}

// atomicBool is a tiny race-safe flag for the test (avoids importing the
// production atomic just for a bool toggle).
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) Store(v bool) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicBool) Load() bool   { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
