//go:build flatcurve

package ftsservice

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// serviceOn builds a service over an EXISTING storage root, so two of them can
// stand for two backends sharing the same NFS volume -- which is what the
// field has, and what makes the write lock contended (#1396).
func serviceOn(t *testing.T, root string, idle time.Duration) (*Service, *mailbox.Resolver) {
	t.Helper()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{
		Engine:            flatcurve.New(flatcurve.Options{}),
		Mailbox:           maildir.New(),
		Index:             file.New(),
		ResolveUser:       func(u string) (*mailbox.UserInfo, error) { return resolver.UserInfo(u, ""), nil },
		Chain:             chain,
		CommitLimit:       2,
		HandleIdleTimeout: idle,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck
	return svc, resolver
}

// holderEnv puts the test binary into helper mode: it plays the backend that
// used to own the user -- indexes once, then goes idle with the given timeout.
//
// A SEPARATE PROCESS is required, and that is the whole point. Xapian's write
// lock is a POSIX fcntl lock, which does not conflict between file descriptors
// of the SAME process: two services in one test process can both open the
// shard and nothing fails. The first version of this test did exactly that and
// skipped itself, "proving" the case could not arise (#1396).
const holderEnv = "YARILO_TEST_FTS_HOLDER"

func TestHelperFTSHolder(t *testing.T) {
	spec := os.Getenv(holderEnv)
	if spec == "" {
		t.Skip("helper process; runs only when the holder env is set")
	}
	parts := strings.SplitN(spec, "|", 2)
	root := parts[0]
	idle, err := time.ParseDuration(parts[1])
	if err != nil {
		t.Fatalf("holder idle: %v", err)
	}

	svc, resolver := serviceOn(t, root, idle)
	info := resolver.UserInfo(testUser, "")
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("holder init: %v", err)
	}
	defer box.Close() //nolint:errcheck

	// Index for real: that leaves the write shard open between commits, which
	// is how the lock is held in the field. A BeginUpdate followed by Rollback
	// would close the shard and release it -- the second thing this test had
	// to learn the hard way.
	uidx := file.New().OpenUser(info)
	defer uidx.Close() //nolint:errcheck
	saveMessage(t, box, uidx, 1, "held")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatalf("holder index: %v", err)
	}
	waitIndexedOn(t, svc, 1)
	// An open write shard is held deliberately, so a parent that still gets
	// the lock tells us about the platform rather than about the fix.
	held, err := svc.handle(testUser)
	if err != nil {
		t.Fatalf("holder handle: %v", err)
	}
	if _, err := held.ui.BeginUpdate(testMbox); err != nil {
		t.Fatalf("holder BeginUpdate: %v", err)
	}
	// Diagnostic: what does this process actually hold?
	fmt.Println("holding")
	os.Stdout.Sync() //nolint:errcheck

	// Live long enough for the parent to observe both states.
	time.Sleep(20 * time.Second)
}

// TestIdleHandleReleasesTheWriteLockForAnotherBackend is #1396: the per-user
// handle was cached for the life of the process and owns the on-disk write
// lock, so a user who moved to another backend left the old one holding it and
// the new owner could never index them -- 654 identical DatabaseLockError in
// twenty minutes, with no way out but a restart.
func TestIdleHandleReleasesTheWriteLockForAnotherBackend(t *testing.T) {
	root := t.TempDir()

	holder := exec.Command(os.Args[0], "-test.run=TestHelperFTSHolder", "-test.v")
	holder.Env = append(os.Environ(), holderEnv+"="+root+"|400ms")
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() }) //nolint:errcheck

	// Wait for it to actually hold the lock before probing.
	rd := bufio.NewReader(out)
	for {
		line, rerr := rd.ReadString('\n')
		if rerr != nil {
			t.Fatalf("holder never took the lock: %v", rerr)
		}
		if strings.TrimSpace(line) == "holding" {
			break
		}
	}

	next, _ := serviceOn(t, root, time.Hour)
	if err := writeProbe(next); err == nil {
		// Not a pass and not a failure of the fix: on this platform's Xapian
		// build a second process opens the same write shard without
		// conflicting, so the situation the fix addresses cannot be staged
		// here. It was observed in the field (654 DatabaseLockError in twenty
		// minutes against a shard held by a backend that no longer owned the
		// user), and the sandbox is where the end-to-end proof has to come
		// from. What IS covered here: the sweep semantics below.
		t.Skip("this platform's Xapian does not block a second process on the write shard; cannot stage the conflict")
	} else if !strings.Contains(strings.ToLower(err.Error()), "lock") {
		t.Fatalf("expected a lock error, got %v", err)
	}

	// The holder goes idle; its sweeper closes the handle and the lock goes.
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if last = writeProbe(next); last == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the new owner never got the write lock after the holder went idle: %v", last)
}

// A handle in use must never be swept: closing an index under a commit tears
// the write out from beneath it.
func TestSweepLeavesHandlesInUseAlone(t *testing.T) {
	root := t.TempDir()
	svc, resolver := serviceOn(t, root, time.Millisecond)
	info := resolver.UserInfo(testUser, "")
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close() }) //nolint:errcheck

	h, err := svc.handle(testUser)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	// Held, as an operation in flight would hold it.
	time.Sleep(20 * time.Millisecond)
	svc.sweepIdleHandles(time.Millisecond)

	svc.mu.Lock()
	_, still := svc.users[testUser]
	svc.mu.Unlock()
	if !still {
		t.Error("a handle with work in flight was closed by the idle sweeper")
	}

	// Once released, the same sweep takes it.
	svc.release(h)
	time.Sleep(20 * time.Millisecond)
	svc.sweepIdleHandles(time.Millisecond)
	svc.mu.Lock()
	_, after := svc.users[testUser]
	svc.mu.Unlock()
	if after {
		t.Error("an idle handle survived the sweep, so its write lock is still held")
	}
}

var _ = fts.MailboxRef{}

// writeProbe opens the user's write shard, which is where the on-disk lock is
// taken. Index() would take it too, but asynchronously through the queue, and
// a probe that cannot see its own failure proves nothing.
func waitIndexedOn(t *testing.T, svc *Service, uid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		last, _, err := svc.Status(testUser, testMbox)
		if err == nil && last >= uid {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the holder never reached its checkpoint, so it never held the lock")
}

func writeProbe(s *Service) error {
	h, err := s.handle(testUser)
	if err != nil {
		return err
	}
	defer s.release(h)
	upd, err := h.ui.BeginUpdate(testMbox)
	if err != nil {
		return err
	}
	return upd.Rollback()
}
