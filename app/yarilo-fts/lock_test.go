package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// ftsLockService runs the real service: the site is a wire field, and a fake
// locker would assert this test's own idea of it.
func ftsLockService(t *testing.T) func() locks.Locker {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-fts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend()
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, derr := net.DialTimeout("unix", sock, 50*time.Millisecond); derr == nil {
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
	return func() locks.Locker {
		c, cerr := locks.NewClient(context.Background(), locks.DialUnix(sock))
		if cerr != nil {
			t.Fatal(cerr)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

// An indexing pass announces what it is doing: it took the key with a bare
// context, and no test reached this path to notice (#1679).
func TestAnIndexingPassNamesItsSite(t *testing.T) {
	dial := ftsLockService(t)
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		err := lockMailbox(dial())("u1@example.com", "INBOX", func() error {
			close(held)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("indexing pass: %v", err)
		}
	}()
	<-held
	defer close(release)

	// Read off a refusal, which is where an operator reads it too.
	other := dial()
	ctx := locks.WithSite(context.Background(), "read")
	lk, err := other.Lock(ctx, locks.FTSKey("u1@example.com", "INBOX"),
		locks.Owner("u1@example.com", "waiter"), time.Minute)
	if !errors.Is(err, locks.ErrBusy) {
		t.Fatalf("second acquisition returned %v, want ErrBusy", err)
	}
	if lk.Site != "fts-index" {
		t.Errorf("an indexing pass holds the key as %q, want \"fts-index\"", lk.Site)
	}
}
