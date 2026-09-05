package mdbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// failingIndex fails the scan's index read, which is how a heal fails without a
// corrupt file: what is under test is the gate, not the scan.
type failingIndex struct {
	mailbox.UserIndex
	err error
}

func (f *failingIndex) GetMessages(uint64, mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return nil, f.err
}

func (f *failingIndex) ExpungeMessage(uint64, uint32) error { return nil }

// mutexLocker grants each resource to one holder at a time and blocks the rest,
// which is the property of the service this test depends on.
type mutexLocker struct {
	mu   sync.Mutex
	held map[string]*sync.Mutex
}

func (l *mutexLocker) forResource(r string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[string]*sync.Mutex{}
	}
	if l.held[r] == nil {
		l.held[r] = &sync.Mutex{}
	}
	return l.held[r]
}

func (l *mutexLocker) Lock(_ context.Context, resource, _ string, _ time.Duration) (locks.Lock, error) {
	l.forResource(resource).Lock()
	return locks.Lock{ID: resource, Resource: resource}, nil
}

func (l *mutexLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}

func (l *mutexLocker) Unlock(_ context.Context, id string) error {
	l.forResource(id).Unlock()
	return nil
}
func (l *mutexLocker) Renew(context.Context, string, time.Duration) error { return nil }
func (l *mutexLocker) HoldsResource(string) bool                          { return false }
func (l *mutexLocker) Close() error                                       { return nil }
func (l *mutexLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *mutexLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *mutexLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

func healTestUser(t *testing.T) (*Backend, *userMailbox) {
	t.Helper()
	b := New(WithLocker(&mutexLocker{}))
	u := b.OpenUser(&mailbox.UserInfo{
		Username: "alice@example.com", Home: t.TempDir(), SessionID: "s1",
	}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	return b, u
}

// A heal that failed is not attempted again until the generation moves: the
// bound used to live in the session, so a reconnect reset it (#1682).
func TestASecondSessionDoesNotRepeatAFailedHeal(t *testing.T) {
	healBarrier.Range(func(k, _ any) bool { healBarrier.Delete(k); return true })
	b, first := healTestUser(t)
	idx := &failingIndex{err: errors.New("scan incomplete")}
	f := &mailbox.Folder{ID: 7, Name: "INBOX", Fsckd: true}

	if _, err := first.HealCorruptFolder(idx, f); err == nil {
		t.Fatal("the first heal was expected to fail")
	}

	// A second session of the same user: a fresh handle, as a reconnect gives.
	second := b.OpenUser(&mailbox.UserInfo{
		Username: first.username, Home: first.home, SessionID: "s2",
	}).(*userMailbox)
	_, err := second.HealCorruptFolder(idx, f)
	if !errors.Is(err, ErrHealDeferred) {
		t.Fatalf("the second session's heal returned %v, want ErrHealDeferred: a reconnect "+
			"is repeating a scan that already failed with nothing changed since", err)
	}

	// A rebuild moves the generation, which is evidence that something changed.
	m, err := second.openMap()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.BumpRebuildCount(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.HealCorruptFolder(idx, f); errors.Is(err, ErrHealDeferred) {
		t.Error("the heal is still deferred after a rebuild: the barrier does not reset, " +
			"so it is a second \"gave up\" somebody has to clear")
	}
}

// Two folders of one user do not heal at once: mdbox Scan walks the whole
// storage, so the map lock is what keeps one scan running at a time.
func TestTwoFoldersOfOneUserDoNotHealAtOnce(t *testing.T) {
	healBarrier.Range(func(k, _ any) bool { healBarrier.Delete(k); return true })
	_, u := healTestUser(t)
	idx := &failingIndex{err: errors.New("scan incomplete")}

	var inScan, peak atomic.Int32
	beforeHealScan = func() {
		n := inScan.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inScan.Add(-1)
	}
	defer func() { beforeHealScan = nil }()

	var wg sync.WaitGroup
	for _, folder := range []*mailbox.Folder{
		{ID: 7, Name: "INBOX", Fsckd: true},
		{ID: 8, Name: "Archive", Fsckd: true},
	} {
		wg.Add(1)
		go func(f *mailbox.Folder) {
			defer wg.Done()
			_, _ = u.HealCorruptFolder(idx, f)
		}(folder)
	}
	wg.Wait()
	if got := peak.Load(); got > 1 {
		t.Errorf("%d heals scanned the storage at once: the map lock is not held across "+
			"the scan, so two folders scan the whole store twice", got)
	}
}

// orderingLocker records the order in which resources are taken, so the lock
// order is measured rather than read off a comment.
type orderingLocker struct {
	mu    sync.Mutex
	order []string
}

func (l *orderingLocker) Lock(_ context.Context, resource, _ string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, resource)
	return locks.Lock{ID: resource, Resource: resource}, nil
}

func (l *orderingLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}
func (l *orderingLocker) Unlock(context.Context, string) error               { return nil }
func (l *orderingLocker) Renew(context.Context, string, time.Duration) error { return nil }
func (l *orderingLocker) HoldsResource(string) bool                          { return false }
func (l *orderingLocker) Close() error                                       { return nil }
func (l *orderingLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *orderingLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *orderingLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

func (l *orderingLocker) taken() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.order))
	copy(out, l.order)
	return out
}

// Every path that takes both keys takes the map first: inverting one trades the
// loop for a deadlock, so the order is read off what runs (#1682).
func TestBothKeysAreAlwaysTakenMapFirst(t *testing.T) {
	healBarrier.Range(func(k, _ any) bool { healBarrier.Delete(k); return true })
	rec := &orderingLocker{}
	b := New(WithLocker(rec))
	u := b.OpenUser(&mailbox.UserInfo{
		Username: "alice@example.com", Home: t.TempDir(), SessionID: "s1",
	}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	idx := &failingIndex{err: errors.New("scan incomplete")}
	_, _ = u.HealCorruptFolder(idx, &mailbox.Folder{ID: 7, Name: "INBOX", Fsckd: true})

	order := rec.taken()
	mapKey := locks.MdboxMapKey(u.username)
	folderKey := locks.MailboxKey(u.username, "INBOX")
	mapAt, folderAt := -1, -1
	for i, r := range order {
		if r == mapKey && mapAt < 0 {
			mapAt = i
		}
		if r == folderKey && folderAt < 0 {
			folderAt = i
		}
	}
	if mapAt < 0 || folderAt < 0 {
		t.Fatalf("the heal took %v, which is not both keys", order)
	}
	if mapAt > folderAt {
		t.Errorf("the heal took the folder key before the map key (%v): delivery and "+
			"RebuildStorage take the map first, and an inversion deadlocks against them", order)
	}
}

// A successful heal leaves no barrier behind: the next failure must be its own,
// not a deferral against a failure that has already been repaired.
func TestASuccessfulHealClearsTheBarrier(t *testing.T) {
	healBarrier.Range(func(k, _ any) bool { healBarrier.Delete(k); return true })
	_, u := healTestUser(t)
	f := &mailbox.Folder{ID: 7, Name: "INBOX", Fsckd: true}
	if _, err := u.HealCorruptFolder(&failingIndex{err: errors.New("scan incomplete")}, f); err == nil {
		t.Fatal("the first heal was expected to fail")
	}
	m, err := u.openMap()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.BumpRebuildCount(); err != nil {
		t.Fatal(err)
	}
	if _, err := u.HealCorruptFolder(&failingIndex{}, f); err != nil {
		t.Fatalf("the heal after a rebuild: %v", err)
	}
	// Same generation as that success: a failure now is this one's, and must be
	// reported rather than deferred against the earlier, repaired one.
	_, err = u.HealCorruptFolder(&failingIndex{err: errors.New("scan incomplete")}, f)
	if errors.Is(err, ErrHealDeferred) {
		t.Error("a heal after a successful one was deferred: the barrier outlives the " +
			"failure it recorded")
	}
}
