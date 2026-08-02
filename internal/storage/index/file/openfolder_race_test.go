package file

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// raceTestLockServer spins up a real embedded locks.Server on a Unix socket
// and returns a factory for independent locks.Client connections — standing
// in for the distributed cross-process lock every production OpenFolder call
// goes through. Without a real locker, withDistLock runs its fn unguarded
// (documented "no locker wired = tests" fast path), which would silently
// hide the very race this test exists to catch.
func raceTestLockServer(t *testing.T) func() locks.Locker {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-openfolder-race")
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
		c, dialErr := net.DialTimeout("unix", sock, 50*time.Millisecond)
		if dialErr == nil {
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
		c, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

// TestOpenFolderConcurrentFirstOpenNoUIDRegression reproduces #644/#658: in
// production each IMAP connection gets its own *userIndex (session.idx is
// created once per TCP connection in internal/imap/server.go, never shared
// across connections), while LMTP holds one long-lived *userIndex delivering
// many messages in sequence. Smoke-test-style clients reconnect per command
// (dial → login → SELECT/STORE/EXPUNGE → logout), so dozens of brand-new,
// empty-cache *userIndex instances call OpenFolder("INBOX", ...) against the
// same on-disk folder while LMTP is actively delivering to it.
//
// OpenFolder's dedup map only protects repeat calls on the SAME *userIndex;
// it does nothing across instances. Two instances can both reach loadOrInit's
// os.Stat before either has created the base index file, both see
// ErrNotExist, and both call createFresh — the later flush(true) silently
// resets NextUID to 1, discarding every UID the other side already
// allocated. This test drives that pattern directly (mdbox driver, matching
// the live reproduction) and asserts NextUID never regresses below any UID
// already committed by the long-lived delivery backend.
func TestOpenFolderConcurrentFirstOpenNoUIDRegression(t *testing.T) {
	dir := t.TempDir()
	newLocker := raceTestLockServer(t)

	newBackend := func() *userIndex {
		return New(WithLocker(newLocker())).OpenUser(&mailbox.UserInfo{
			Username: testUser, Home: testHome(dir, testUser), Driver: "mdbox",
		}).(*userHandle).ui
	}

	const deliveries = 40
	const shortLivedConns = 40

	lmtp := newBackend()
	var maxCommittedUID atomic.Uint32
	var regressed atomic.Bool
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < deliveries; i++ {
			f, err := lmtp.OpenFolder("INBOX", 0, "")
			if err != nil {
				t.Errorf("lmtp OpenFolder: %v", err)
				return
			}
			uid, err := lmtp.AllocateUID(f.ID)
			if err != nil {
				t.Errorf("lmtp AllocateUID: %v", err)
				return
			}
			if prev := maxCommittedUID.Load(); uid < prev {
				regressed.Store(true)
				t.Errorf("lmtp allocated regressed UID %d after previously committing %d", uid, prev)
			}
			ms, _ := lmtp.NextModSeq(f.ID)
			if err := lmtp.AppendMessage(f.ID, &mailbox.MessageMeta{
				UID: uid, ModSeq: ms, Filename: "d.eml", Size: 100,
			}); err != nil {
				t.Errorf("lmtp AppendMessage uid=%d: %v", uid, err)
				return
			}
			for {
				prev := maxCommittedUID.Load()
				if uid <= prev || maxCommittedUID.CompareAndSwap(prev, uid) {
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Each short-lived connection mimics smoke-test's reconnect-per-command
	// pattern: a fresh *userIndex opens INBOX (no dedup history at all) and
	// does a read-only pass, then discards the instance — exactly what
	// session.idx / OpenUser does per TCP connection in production.
	for i := 0; i < shortLivedConns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := newBackend()
			f, err := conn.OpenFolder("INBOX", 0, "")
			if err != nil {
				t.Errorf("short-lived OpenFolder: %v", err)
				return
			}
			_, _ = conn.GetMessages(f.ID, mailbox.SeqSet{})
		}()
	}

	wg.Wait()

	final := newBackend()
	ff, err := final.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatalf("final OpenFolder: %v", err)
	}
	if got, want := ff.NextUID, maxCommittedUID.Load()+1; got < want {
		t.Errorf("on-disk NextUID regressed: got %d, want >= %d (highest UID ever committed was %d)",
			got, want, maxCommittedUID.Load())
	}
	if regressed.Load() {
		t.Fatal("lmtp observed a UID regression mid-run — see errors above")
	}
}
