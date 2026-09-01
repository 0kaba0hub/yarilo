package maildir

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// countingLocker records how many times a resource is actually acquired, which
// is the number this change exists to bring down.
type countingLocker struct {
	locks.Locker
	acquires atomic.Int64
	held     map[string]bool
}

func (l *countingLocker) Lock(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	l.acquires.Add(1)
	if l.held == nil {
		l.held = map[string]bool{}
	}
	l.held[resource] = true
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

func (l *countingLocker) Unlock(ctx context.Context, id string) error {
	delete(l.held, id)
	return nil
}

func (l *countingLocker) HoldsResource(resource string) bool { return l.held[resource] }

func batchBox(t *testing.T) (*userMailbox, *countingLocker) {
	t.Helper()
	root := t.TempDir()
	const user = "u@x.com"
	l := &countingLocker{}
	info := &mailbox.UserInfo{Username: user, Home: testHome(root, user)}
	box := New(WithLocker(l)).OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	return box, l
}

// One STORE, one acquisition of the folder lock.
//
// Per message it was one each: a command over 200 messages took the
// cross-process lock 200 times, and under a second path holding the same lock
// -- a SELECT's reconcile -- that is what produced the stalls (#1623).
func TestABatchTakesTheFolderLockOnce(t *testing.T) {
	box, l := batchBox(t)
	const n = 20
	writes := make([]mailbox.FlagWrite, 0, n)
	for i := 0; i < n; i++ {
		name := "170000000" + string(rune('0'+i%10)) + ".M1P" + string(rune('a'+i)) + ".host:2,"
		deliverToCur(t, box, name, "From: a@b\r\n\r\nx\r\n")
		writes = append(writes, mailbox.FlagWrite{
			UID: uint32(i + 1), Filename: name, Flags: []string{`\Seen`}, Keywords: []string{"$Important"},
		})
	}
	before := l.acquires.Load()
	results := box.WriteFlagsMulti("INBOX", writes)
	got := l.acquires.Load() - before

	if got != 1 {
		t.Errorf("the batch took the folder lock %d times, want 1", got)
	}
	if len(results) != n {
		t.Fatalf("got %d results for %d writes", len(results), n)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("uid %d: %v", r.UID, r.Err)
		}
		if !strings.Contains(r.Filename, "S") {
			t.Errorf("uid %d: name %q carries no \\Seen", r.UID, r.Filename)
		}
	}
}

// A message whose file is gone does not take the rest of the batch with it.
func TestABatchWithOneMissingFileWritesTheRest(t *testing.T) {
	box, _ := batchBox(t)
	names := []string{
		"1700000001.M1Pa.host:2,",
		"1700000002.M1Pb.host:2,",
		"1700000003.M1Pc.host:2,",
	}
	for _, n := range names {
		deliverToCur(t, box, n, "From: a@b\r\n\r\nx\r\n")
	}
	writes := []mailbox.FlagWrite{
		{UID: 1, Filename: names[0], Flags: []string{`\Seen`}},
		{UID: 2, Filename: "1700000099.MgoneP.host:2,", Flags: []string{`\Seen`}},
		{UID: 3, Filename: names[2], Flags: []string{`\Seen`}},
	}
	results := box.WriteFlagsMulti("INBOX", writes)
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("result %d carries an error: %v", i, r.Err)
		}
	}
	// The two that exist were renamed; the missing one keeps the name the
	// index has, for the reconcile to settle.
	for _, i := range []int{0, 2} {
		if !strings.Contains(results[i].Filename, "S") {
			t.Errorf("uid %d: name %q was not written", results[i].UID, results[i].Filename)
		}
	}
	if results[1].Filename != writes[1].Filename {
		t.Errorf("the missing message came back as %q", results[1].Filename)
	}
}
