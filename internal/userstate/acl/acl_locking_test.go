package acl

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Set must repair a folder whose yarilo-acl file is corrupt. #1140 p.5 makes
// "one bad line fails the whole file" reachable, and before this Set went
// through Update's loadLocked, which failed on the parse -- leaving no product
// path to fix a corrupt file. The constant function ignores the current value,
// so Update no longer requires the load to succeed for it.
func TestSet_RepairsCorruptFile(t *testing.T) {
	s := New(t.TempDir(), "", "", "/", "", "alice", "test.bin/1/alice@example.com/sess1", Policy{}, nil)
	path := s.Path("Broken")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("!!not a valid acl line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Precondition: the file is genuinely unreadable.
	if _, err := s.Get("Broken"); err == nil {
		t.Fatal("expected Get to fail on the corrupt file")
	}

	// Set repairs it.
	bob := mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}
	if err := s.Set("Broken", mailbox.ACL{{Identifier: bob, Rights: "lr"}}); err != nil {
		t.Fatalf("Set on a corrupt file: %v", err)
	}
	got, err := s.Get("Broken")
	if err != nil {
		t.Fatalf("Get after repair: %v", err)
	}
	if len(got) != 1 || string(got[0].Rights) != "lr" {
		t.Errorf("after repair: %v, want a single bob lr", got)
	}

	// But a function that declines to replace (returns nil) surfaces the
	// corruption instead of a silent OK.
	if err := os.WriteFile(path, []byte("!!broken again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("Broken", func(mailbox.ACL) (mailbox.ACL, error) { return nil, nil }); err == nil {
		t.Error("Update returning nil on a corrupt file must surface the corruption, not report success")
	}
}

// ListRebuild must resolve every folder's ACL before taking the list lock: the
// resolve callback takes the folder lock (Store.Get), so holding the list lock
// across it is list->folder, the reverse of Update's folder->list, and the two
// deadlock (#1147). A nil locker cannot show this, so the lock order needs a
// locker that tracks holds.
func TestListRebuild_ResolvesBeforeListLock(t *testing.T) {
	lk := &trackingLocker{held: map[string]bool{}}
	s := New(t.TempDir(), "", "", "/", "", "alice", "test.bin/1/alice@example.com/sess1", Policy{}, lk)
	listKey := locks.ACLListKey(s.mailboxesRoot())

	heldDuringResolve := false
	err := s.ListRebuild([]string{"Foo", "Bar"}, func(folder string) (mailbox.ACL, error) {
		if lk.HoldsResource(listKey) {
			heldDuringResolve = true
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("ListRebuild: %v", err)
	}
	if heldDuringResolve {
		t.Error("ListRebuild held the list lock while resolving folders: list->folder inverts Update's folder->list and deadlocks a concurrent SETACL (#1147)")
	}
}

// trackingLocker is a minimal in-process Locker that records which resources are
// held, so a test can assert lock ordering. Lock IDs equal the resource name.
type trackingLocker struct {
	held map[string]bool
}

func (l *trackingLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	l.held[resource] = true
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

func (l *trackingLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, resource, owner, ttl)
}

func (l *trackingLocker) Unlock(_ context.Context, lockID string) error {
	delete(l.held, lockID)
	return nil
}

func (l *trackingLocker) Renew(context.Context, string, time.Duration) error { return nil }

func (l *trackingLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}

func (l *trackingLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }

func (l *trackingLocker) HoldsResource(resource string) bool { return l.held[resource] }

func (l *trackingLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

func (l *trackingLocker) Close() error { return nil }
