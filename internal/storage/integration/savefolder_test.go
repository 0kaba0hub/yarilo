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

	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestSaveFolderDoesNotOverwriteFreshNextUID is the regression for the
// Phase 2.5 fix. Scenario:
//
//   - Process B opens the folder, captures NextUID = N in its local Folder
//     snapshot.
//   - Process A appends K messages via AllocateAppend, bumping the on-disk
//     NextUID to N+K.
//   - Process B then calls SaveFolder with its stale snapshot (NextUID still
//     N). Before the fix, this overwrote the disk's N+K back to N, causing
//     the next AllocateAppend to hand out a duplicate UID.
//
// After the fix, SaveFolder re-reads the header under the X lock and only
// persists fields the caller owns (Messages) — NextUID stays at N+K.
func TestSaveFolderDoesNotOverwriteFreshNextUID(t *testing.T) {
	dialLocker := embeddedLocksForSaveTest(t)

	homeDir := t.TempDir()
	user := &mailbox.UserInfo{Username: "bob@example.com", Home: homeDir}

	// Two "processes" pointing at the same on-disk maildir + index.
	lkA := dialLocker()
	mbA := maildir.New(maildir.WithLocker(lkA)).OpenUser(user)
	ixA := file.New(file.WithLocker(lkA)).OpenUser(user)
	t.Cleanup(func() { _ = ixA.Close() })
	if err := mbA.Init(); err != nil {
		t.Fatalf("init A: %v", err)
	}

	lkB := dialLocker()
	_ = maildir.New(maildir.WithLocker(lkB)).OpenUser(user) // process B does not write via maildir in this scenario
	ixB := file.New(file.WithLocker(lkB)).OpenUser(user)
	t.Cleanup(func() { _ = ixB.Close() })

	// Both processes open the folder; both observe NextUID = 1.
	folderA, err := ixA.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	folderB, err := ixB.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	if folderA.NextUID != 1 || folderB.NextUID != 1 {
		t.Fatalf("initial NextUID: want 1/1, got %d/%d", folderA.NextUID, folderB.NextUID)
	}

	// Process A appends 3 messages — disk NextUID is now 4.
	for i := 0; i < 3; i++ {
		filename, err := mbA.Save("INBOX", strings.NewReader("body"), 0, nil)
		if err != nil {
			t.Fatalf("save A %d: %v", i, err)
		}
		uid, err := ixA.AllocateAppend(folderA.ID, &mailbox.MessageMeta{Filename: filename})
		if err != nil {
			t.Fatalf("allocate A %d: %v", i, err)
		}
		if err := mbA.AppendUIDEntry("INBOX", uid, filename); err != nil {
			t.Fatalf("uidlist A %d: %v", i, err)
		}
	}

	// Process B persists its stale snapshot. folderB.NextUID is still 1 here
	// (B has not seen A's writes). Without the fix this would clobber disk
	// to NextUID=1 and the next A.AllocateAppend would re-hand-out UID=1.
	folderB.Messages = 7 // any value the caller actually owns
	if err := ixB.SaveFolder(folderB); err != nil {
		t.Fatalf("save folder B: %v", err)
	}

	// Process A appends one more — must get UID 4, not 1.
	filename, err := mbA.Save("INBOX", strings.NewReader("after-save"), 0, nil)
	if err != nil {
		t.Fatalf("save A post: %v", err)
	}
	uid, err := ixA.AllocateAppend(folderA.ID, &mailbox.MessageMeta{Filename: filename})
	if err != nil {
		t.Fatalf("allocate A post: %v", err)
	}
	if uid != 4 {
		t.Fatalf("NextUID corruption: A got UID=%d after B's stale SaveFolder, want 4", uid)
	}
}

// embeddedLocksForSaveTest is a local twin of the helper in
// locks_integration_test.go — kept here so this file is self-contained.
func embeddedLocksForSaveTest(t *testing.T) func() locks.Locker {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-sf")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(5 * time.Millisecond))
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
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
	return func() locks.Locker {
		c, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}
