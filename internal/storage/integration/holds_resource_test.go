package integration_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestHoldsResourceSkipsInnerAcquire is the regression for Phase 2.7's
// re-entrancy fix. POP3 QUIT (and similar batch operations) takes an outer
// X lock on mbox:<user>:INBOX and then calls per-message storage methods.
// Without the HoldsResource skip the storage backends' withMailboxLock
// would try to Acquire the same key from the same Locker, hit ErrBusy,
// retry forever, and the QUIT would never complete.
//
// This test exercises the skip path directly: take an outer lock, then
// call storage write methods that would normally take the same lock, and
// assert they complete promptly without spinning on retry.
func TestHoldsResourceSkipsInnerAcquire(t *testing.T) {
	sock := holdsTestSocket(t)
	lk := newHoldsClient(t, sock)

	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "carol@example.com", Home: home}
	mb := maildir.New(maildir.WithLocker(lk)).OpenUser(user)
	idx := file.New(file.WithLocker(lk)).OpenUser(user)
	t.Cleanup(func() { _ = idx.Close() })
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}

	// Stage one message under per-call locks (normal write path).
	uid, err := idx.AllocateUID(folder.ID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	filename, _, _, err := mb.Save("INBOX", strings.NewReader("body"), uid, 4, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Filename: filename}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Take an outer X lock — same Locker, same key.
	key := locks.MailboxKey("carol@example.com", "INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outer, err := lk.Lock(ctx, key, "test-outer/0/carol@example.com", 30*time.Second)
	if err != nil {
		t.Fatalf("outer lock: %v", err)
	}
	defer func() { _ = lk.Unlock(ctx, outer.ID) }()
	if !lk.HoldsResource(key) {
		t.Fatal("HoldsResource returned false after Lock")
	}

	// Storage call MUST short-circuit through HoldsResource. If it instead
	// tried to Acquire, it would hit ErrBusy and Acquire's retry loop would
	// run until the 35-second timeout. HoldsResource is per-goroutine, so
	// the inner calls run on the SAME goroutine as the outer Lock —
	// matching POP3 QUIT's shape (outer caller + inner storage methods are
	// synchronous on one goroutine). We bracket the inner calls with a
	// wall-clock deadline so a regression (Acquire actually firing and
	// blocking on ErrBusy) surfaces as a clear test failure rather than a
	// CI-killing 30-second hang.
	start := time.Now()
	if err := mb.Remove("INBOX", filename); err != nil {
		t.Errorf("inner remove (should skip lock and succeed): %v", err)
	}
	if err := idx.ExpungeMessage(folder.ID, uid); err != nil {
		t.Errorf("inner expunge (should skip lock and succeed): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("inner storage calls took %v — HoldsResource skip not engaging", elapsed)
	}

	// After releasing the outer lock, HoldsResource flips back to false.
	if err := lk.Unlock(ctx, outer.ID); err != nil {
		t.Fatalf("outer unlock: %v", err)
	}
	if lk.HoldsResource(key) {
		t.Fatal("HoldsResource still true after Unlock — holds map not pruned")
	}
}

// --- helpers --------------------------------------------------------------

func holdsTestSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-hr")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(5 * time.Millisecond))
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, ln)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", sock, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	})
	return sock
}

func newHoldsClient(t *testing.T, sock string) locks.Locker {
	t.Helper()
	c, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
