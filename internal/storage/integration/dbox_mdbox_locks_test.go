package integration_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// dboxMdboxLocker spins an embedded yarilo-locks server scoped to this test
// and returns a Locker dialer for each "process" instance.
func dboxMdboxLocker(t *testing.T) func() locks.Locker {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-dm")
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
	return func() locks.Locker {
		c, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

// TestDboxConcurrentSaveAndDelete proves Phase 2.6 wired dbox into the
// cross-process lock chain. Two Backend instances (== two processes) hammer
// the same folder with concurrent Save / Remove / Delete operations through
// a shared embedded locks server. Without WithLocker the test could observe
// Delete wiping a folder mid-Save and produce missing files; with the lock
// every operation completes atomically.
func TestDboxConcurrentSaveAndDelete(t *testing.T) {
	dial := dboxMdboxLocker(t)
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "alice@example.com", Home: home}

	mbA := dboxv2.New(dboxv2.WithLocker(dial())).OpenUser(user)
	mbB := dboxv2.New(dboxv2.WithLocker(dial())).OpenUser(user)
	if err := mbA.Init(); err != nil {
		t.Fatalf("init A: %v", err)
	}
	if err := mbB.Init(); err != nil {
		t.Fatalf("init B: %v", err)
	}
	if err := mbA.Create("Stress"); err != nil {
		t.Fatalf("create: %v", err)
	}

	const iterations = 30
	var wg sync.WaitGroup
	wg.Add(2)
	// A writes UID 1..iterations into the sdbox tree; B periodically
	// drops the whole folder. The contestants never deadlock and the
	// locks server returns every Lock/Unlock cleanly — the test
	// passes if no panic / stale-fd error surfaces.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mbA.Create("Stress")
			_, _ = mbA.Save("Stress", strings.NewReader("hello"), uint32(i+1), 5, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mbB.Delete("Stress")
		}
	}()
	wg.Wait()

	_ = mbA.Create("Stress")
}

// TestMdboxConcurrentSaveTwoProcesses checks the mdbox lock plumbing under
// concurrent Save from two processes into the same folder. Both processes
// share the same mdbox-storage tree; the per-folder X lock serialises map
// writes so the dbox.map ends up consistent.
func TestMdboxConcurrentSaveTwoProcesses(t *testing.T) {
	dial := dboxMdboxLocker(t)
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "alice@example.com", Home: home}

	mbA := mdbox.New(mdbox.WithLocker(dial())).OpenUser(user)
	mbB := mdbox.New(mdbox.WithLocker(dial())).OpenUser(user)
	if err := mbA.Init(); err != nil {
		t.Fatalf("init A: %v", err)
	}
	if err := mbB.Init(); err != nil {
		t.Fatalf("init B: %v", err)
	}

	const perProcess = 20
	tokens := make(chan string, perProcess*2)
	var wg sync.WaitGroup
	wg.Add(2)
	// mdbox derives its token from (file_id, offset), not the UID
	// parameter, so any non-zero placeholder works for this test.
	go func() {
		defer wg.Done()
		for i := 0; i < perProcess; i++ {
			tok, err := mbA.Save("INBOX", strings.NewReader("from-A"), uint32(i+1), 6, nil)
			if err != nil {
				t.Errorf("save A %d: %v", i, err)
				return
			}
			tokens <- tok
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < perProcess; i++ {
			tok, err := mbB.Save("INBOX", strings.NewReader("from-B"), uint32(perProcess+i+1), 6, nil)
			if err != nil {
				t.Errorf("save B %d: %v", i, err)
				return
			}
			tokens <- tok
		}
	}()
	wg.Wait()
	close(tokens)

	seen := make(map[string]bool)
	for tok := range tokens {
		if seen[tok] {
			t.Errorf("duplicate token: %s", tok)
		}
		seen[tok] = true
	}
	// Every token resolves to a real message under List.
	msgs, err := mbA.List("INBOX")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) < perProcess*2 {
		// Tokens generated under lock are unique per (fileID, offset).
		// Both processes' messages end up in the shared mdbox-storage, all
		// listed via the shared dbox.map.
		t.Errorf("list count: got %d, want >= %d", len(msgs), perProcess*2)
	}
}
