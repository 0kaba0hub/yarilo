package msgcache

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// lockServer starts a real lock service: shared and exclusive have to mean what
// the service says they mean, and a recording double would only repeat this
// test's own assumption back to it.
func lockServer(t *testing.T) func() locks.Locker {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(5 * time.Millisecond))
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

func sharedFolder(t *testing.T, lk locks.Locker) (mailbox.UserIndex, uint64) {
	t.Helper()
	idx := file.New().OpenUser(&mailbox.UserInfo{
		Username: "u@example.com", Home: t.TempDir(), SessionID: "sess",
	})
	f, err := idx.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1}); err != nil {
		t.Fatal(err)
	}
	// Warm the cache file into existence: a shared open that has to create it
	// reopens exclusively, and this test is about the warm path every FETCH
	// after the first one takes.
	warm := Open(idx, f.ID, Options{Locker: lk, User: "u@example.com", SessionID: "warm", Folder: "INBOX"})
	if warm == nil {
		t.Fatal("no cache handle")
	}
	warm.Close()
	return idx, f.ID
}

// Two readers of one folder do not refuse each other.
//
// A FETCH opened the cache with the mailbox key held exclusively, so two
// sessions reading the same folder queued behind one another -- with holds of
// 12 ms and waits reaching 46 s, because the loser of a draw simply draws again
// (#1673). Reading takes the key shared now.
func TestTwoReadersShareTheFolder(t *testing.T) {
	newLocker := lockServer(t)
	lockA, lockB := newLocker(), newLocker()
	idx, fid := sharedFolder(t, lockA)

	ro := Options{Locker: lockA, User: "u@example.com", SessionID: "one", Folder: "INBOX", Shared: true}
	first := Open(idx, fid, ro)
	if first == nil {
		t.Fatal("no cache handle for the first reader")
	}
	defer first.Close()

	second := make(chan *Handle, 1)
	roB := ro
	roB.Locker, roB.SessionID = lockB, "two"
	go func() { second <- Open(idx, fid, roB) }()
	select {
	case h := <-second:
		if h == nil {
			t.Fatal("the second reader got no handle")
		}
		h.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("a second reader waited on the first: the read path is holding the folder key " +
			"exclusively, so concurrent FETCHes serialise")
	}
}

// A FETCH that sets \Seen still excludes a reader.
//
// The sharing above must not reach the path that writes flags: BODY[] without
// PEEK is a write, and a reader must not run beside it. Asserted from the other
// direction so the change cannot be "share everything".
func TestAWriterStillExcludesAReader(t *testing.T) {
	newLocker := lockServer(t)
	lockA, lockB := newLocker(), newLocker()
	idx, fid := sharedFolder(t, lockA)

	writer := Open(idx, fid, Options{Locker: lockA, User: "u@example.com", SessionID: "seen", Folder: "INBOX"})
	if writer == nil {
		t.Fatal("no cache handle for the writer")
	}
	reader := make(chan *Handle, 1)
	go func() {
		reader <- Open(idx, fid, Options{
			Locker: lockB, User: "u@example.com", SessionID: "reader", Folder: "INBOX", Shared: true,
		})
	}()
	select {
	case <-reader:
		t.Fatal("a reader ran while a \\Seen-setting FETCH held the folder: the write is not excluding")
	case <-time.After(300 * time.Millisecond):
	}
	writer.Close()
	select {
	case h := <-reader:
		if h == nil {
			t.Fatal("the reader got no handle after the writer released")
		}
		h.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("the reader never proceeded after the writer released")
	}
}
