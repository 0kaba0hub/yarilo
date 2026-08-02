// Package integration verifies that cross-process write coordination via
// pkg/locks prevents the UID-race and index corruption documented in
// ARCHITECTURE.md §Known issues. Two Backend instances pointing at the same
// on-disk maildir + index, with a shared embedded yarilo-locks server, must
// serialise concurrent APPEND operations across processes.
package integration_test

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// shortSocket returns a Unix-socket path short enough for macOS/BSD
// sockaddr_un.sun_path (104 bytes). t.TempDir() under /var/folders is too long.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "l.sock")
}

// embeddedLocks spins up an in-process MemoryBackend + Unix-socket Server
// and returns a Locker dialer. The cleanup tears it all down.
func embeddedLocks(t *testing.T) func() locks.Locker {
	t.Helper()
	sock := shortSocket(t)
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
	// Wait for the listener to accept.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sock, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
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
			t.Fatalf("dial locks: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}

// twoProcessAppend stresses two Backend instances writing to the same
// on-disk maildir + index concurrently. Each "process" runs N appender
// goroutines. With shared yarilo-locks they must agree on UID sequencing
// (no duplicates) and produce a consistent uidlist + index.
func TestTwoProcessAppendNoUIDCollision(t *testing.T) {
	dialLocker := embeddedLocks(t)

	homeDir := t.TempDir()
	user := &mailbox.UserInfo{Username: "alice@example.com", Home: homeDir}

	const (
		procs         = 2
		gorPerProc    = 4
		appendsPerGor = 10
	)
	totalAppends := procs * gorPerProc * appendsPerGor

	// Create per-process Backend pairs. Each gets its own Locker client
	// (== its own connection), but they all point at the same embedded
	// locks server. From the locks server's view, every connection is a
	// distinct "process".
	type proc struct {
		mb mailbox.UserMailbox
		ix *file.Backend
	}
	procPair := make([]proc, procs)
	for i := 0; i < procs; i++ {
		lk := dialLocker()
		mbBackend := maildir.New(maildir.WithLocker(lk))
		ixBackend := file.New(file.WithLocker(lk))
		mb := mbBackend.OpenUser(user)
		if err := mb.Init(); err != nil {
			t.Fatalf("init: %v", err)
		}
		procPair[i] = proc{mb: mb, ix: ixBackend}
	}

	// Each process opens its own *userIndex pointing at the same on-disk
	// files. OpenFolder is once per process (not per goroutine).
	var (
		wg       sync.WaitGroup
		seenUIDs sync.Map // uid → struct{}{}
		errCh    = make(chan error, totalAppends)
	)

	for p := 0; p < procs; p++ {
		idx := procPair[p].ix.OpenUser(user)
		t.Cleanup(func() { _ = idx.Close() })
		folder, err := idx.OpenFolder("INBOX", 1)
		if err != nil {
			t.Fatalf("open folder p%d: %v", p, err)
		}
		mb := procPair[p].mb

		for g := 0; g < gorPerProc; g++ {
			wg.Add(1)
			go func(pid, gid int) {
				defer wg.Done()
				for k := 0; k < appendsPerGor; k++ {
					// 1. Allocate the next UID through the index lock.
					uid, err := idx.AllocateUID(folder.ID)
					if err != nil {
						errCh <- err
						return
					}
					// Duplicate-UID check.
					if _, dup := seenUIDs.LoadOrStore(uid, struct{}{}); dup {
						errCh <- &dupErr{uid: uid}
						return
					}
					// 2. Save the message file under the allocated UID
					//    (maildir writes uidlist inline; sdbox renames to u.<uid>).
					content := strings.NewReader("hello from p" +
						strconv.Itoa(pid) + ":g" + strconv.Itoa(gid) + ":k" + strconv.Itoa(k))
					filename, _, _, err := mb.Save("INBOX", content, uid, 0, nil, [16]byte{})
					if err != nil {
						errCh <- err
						return
					}
					// 3. Record the meta under the same lock.
					if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
						UID:      uid,
						Filename: filename,
						Flags:    []string{},
					}); err != nil {
						errCh <- err
						return
					}
				}
			}(p, g)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("appender failed: %v", err)
	}

	// Verify uidlist contains exactly totalAppends entries, all distinct.
	// With no mail_path the maildir driver defaults to <home>/Maildir and
	// INBOX is that root, so the uidlist sidecar lives there.
	uidlistPath := filepath.Join(homeDir, "Maildir", "yarilo-uidlist")
	f, err := os.Open(uidlistPath)
	if err != nil {
		t.Fatalf("open uidlist: %v", err)
	}
	defer f.Close()
	uidsSeen := make(map[uint32]bool)
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if !strings.HasPrefix(line, "3 V") {
				t.Fatalf("uidlist missing v3 header: %q", line)
			}
			continue
		}
		colon := strings.IndexByte(line, ' ')
		if colon < 0 {
			t.Fatalf("malformed uidlist line: %q", line)
		}
		u, err := strconv.ParseUint(line[:colon], 10, 32)
		if err != nil {
			t.Fatalf("parse uid %q: %v", line[:colon], err)
		}
		if uidsSeen[uint32(u)] {
			t.Fatalf("duplicate uid in uidlist: %d", u)
		}
		uidsSeen[uint32(u)] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan uidlist: %v", err)
	}
	if len(uidsSeen) != totalAppends {
		t.Fatalf("uidlist count: got %d, want %d", len(uidsSeen), totalAppends)
	}
}

type dupErr struct{ uid uint32 }

func (e *dupErr) Error() string { return "duplicate uid: " + strconv.FormatUint(uint64(e.uid), 10) }

// BenchmarkAcquireRelease measures one cross-process LOCK/UNLOCK pair against
// the embedded yarilo-locks server. Target: < 500 µs per pair per the Phase 1
// plan (see the internal docs). Reports allocations and ns/op.
func BenchmarkAcquireRelease(b *testing.B) {
	dialLocker := embeddedLocksB(b)
	lk := dialLocker()
	ctx := context.Background()
	resource := "mbox:bench:INBOX"
	owner := "bench/0/x"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l, err := lk.Lock(ctx, resource, owner, 30*time.Second)
		if err != nil {
			b.Fatalf("lock: %v", err)
		}
		if err := lk.Unlock(ctx, l.ID); err != nil {
			b.Fatalf("unlock: %v", err)
		}
	}
}

// embeddedLocksB is the testing.B-compatible variant of embeddedLocks.
func embeddedLocksB(b *testing.B) func() locks.Locker {
	b.Helper()
	dir, err := os.MkdirTemp("", "yl")
	if err != nil {
		b.Fatalf("mkdir temp: %v", err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(50 * time.Millisecond))
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		b.Fatalf("listen unix: %v", err)
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
	b.Cleanup(func() {
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	})
	return func() locks.Locker {
		c, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		b.Cleanup(func() { _ = c.Close() })
		return c
	}
}
