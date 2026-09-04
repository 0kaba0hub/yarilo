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

// lockServer starts a real lock service: a recording double would only repeat
// this test's own assumption about the modes back to it.
func lockServer(t *testing.T) func() locks.Locker {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-mc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(5 * time.Millisecond))
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, lerr := locks.ListenUnix(sock)
	if lerr != nil {
		t.Fatal(lerr)
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
	// Warm the file into existence: a shared open that must create it reopens
	// exclusively, and the warm path is what every later FETCH takes.
	warm := Open(idx, f.ID, Options{Locker: lk, User: "u@example.com", SessionID: "warm", Folder: "INBOX"})
	if warm == nil {
		t.Fatal("no cache handle")
	}
	warm.Close()
	return idx, f.ID
}

// A reader opens while another reader holds the folder. The holder is an
// independent client: a deferred handle releases too early to overlap (#1673).
func TestAReaderOpensWhileAnotherReaderHoldsTheFolder(t *testing.T) {
	newLocker := lockServer(t)
	lockA, lockB := newLocker(), newLocker()
	idx, fid := sharedFolder(t, lockA)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, err := locks.AcquireShared(ctx, lockB, locks.MailboxKey("u@example.com", "INBOX"),
		locks.Owner("u@example.com", "other-reader"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lockB.Unlock(ctx, held.ID) //nolint:errcheck

	opened := make(chan *Handle, 1)
	go func() {
		opened <- Open(idx, fid, Options{
			Locker: lockA, User: "u@example.com", SessionID: "one", Folder: "INBOX",
			Shared: true, DeferWrites: true,
		})
	}()
	select {
	case h := <-opened:
		if h == nil {
			t.Fatal("the reader got no handle")
		}
		h.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("a reader waited on another reader: the read path is taking the folder key " +
			"exclusively, so concurrent FETCHes serialise")
	}
}

// A FETCH that sets \Seen still excludes a reader: BODY[] without PEEK is a
// write. Asserted from the other side so the change cannot be "share all".
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
			Locker: lockB, User: "u@example.com", SessionID: "reader", Folder: "INBOX",
			Shared: true, DeferWrites: true,
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

// Shared without DeferWrites is refused: an undeferred handle writes through a
// live descriptor, and a shared key excludes no writer (#1673).
func TestSharedWithoutDeferredWritesIsRefused(t *testing.T) {
	newLocker := lockServer(t)
	lk := newLocker()
	idx, fid := sharedFolder(t, lk)
	defer func() {
		if recover() == nil {
			t.Error("Open accepted Shared without DeferWrites: the cache would be written under a shared key")
		}
	}()
	_ = Open(idx, fid, Options{Locker: lk, User: "u@example.com", SessionID: "s", Folder: "INBOX", Shared: true})
}

// The in-process tier shares too: two sessions of one user usually share a pod,
// and a mutex here serialises them before the service is asked (#1673).
func TestTheInProcessTierSharesToo(t *testing.T) {
	newLocker := lockServer(t)
	lk := newLocker()
	idx, fid := sharedFolder(t, lk)
	path, err := idx.(Index).CachePath(fid)
	if err != nil {
		t.Fatal(err)
	}
	release := lockCachePath(path, true) // another reader, mid-open
	defer release()

	opened := make(chan *Handle, 1)
	go func() {
		opened <- Open(idx, fid, Options{
			Locker: lk, User: "u@example.com", SessionID: "one", Folder: "INBOX",
			Shared: true, DeferWrites: true,
		})
	}()
	select {
	case h := <-opened:
		if h == nil {
			t.Fatal("the reader got no handle")
		}
		h.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("a reader waited on another reader inside one process: the cache path lock is " +
			"exclusive, so two sessions in one pod serialise before the lock service is asked")
	}
}
