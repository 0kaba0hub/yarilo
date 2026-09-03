package file

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The second session's write reaches the lock service under the second
// session's name.
//
// A userIndex is shared by every session of one user, and its owner was minted
// by whoever created the cache entry. So held_by named the first opener for as
// long as the entry lived -- never the session actually holding the lock, which
// is the one thing an operator reads the field for. It cost two rounds of
// chasing a session bug that was not there (#1664).
func TestASecondSessionLocksUnderItsOwnName(t *testing.T) {
	dir := t.TempDir()
	newLocker := raceTestLockServer(t)
	const user = "two@example.com"
	backend := New(WithLocker(newLocker()))

	open := func(session string) mailbox.UserIndex {
		return backend.OpenUser(&mailbox.UserInfo{
			Username: user, Home: testHome(dir, user), SessionID: session,
		})
	}
	first := open("session-one")
	defer first.Close() //nolint:errcheck
	second := open("session-two")
	defer second.Close() //nolint:errcheck

	// The first session creates the shared state; the second inherits it.
	f, err := first.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10}); err != nil {
		t.Fatal(err)
	}

	g, err := second.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := counterVal(t, metricLockAcquired, "exclusive", lockSiteWrite)
	if err := second.AppendMessage(g.ID, &mailbox.MessageMeta{UID: 2, Filename: "2", Size: 10}); err != nil {
		t.Fatal(err)
	}
	// The write has to have gone to the service, or the name it announced was
	// never sent and this asserts nothing.
	if got := counterVal(t, metricLockAcquired, "exclusive", lockSiteWrite) - before; got != 1 {
		t.Fatalf("the second session's append took %v exclusive acquisitions, want 1", got)
	}

	shared := backend.users[cacheKey(&mailbox.UserInfo{Username: user, Home: testHome(dir, user)})].ui
	fs := shared.open[g.ID]
	// Read with the shared index's own owner as the fallback, which is what
	// withDistLock passes: unstamped, this returns the first session's name --
	// the exact string the field reported for every session after the first.
	got := fs.lockOwner(shared.owner)
	want := locks.Owner(user, "session-two")
	if got != want {
		t.Errorf("the second session's write announced %q, want %q -- held_by names whoever opened "+
			"the shared index, not the session holding the lock", got, want)
	}
	if other := locks.Owner(user, "session-one"); got == other {
		t.Errorf("the second session announced the first session's name %q", other)
	}
}

// A path that reaches the index without a handle still announces something.
func TestAnIndexWithoutAStampedFolderFallsBackToItsOwnOwner(t *testing.T) {
	dir := t.TempDir()
	const user = "fallback@example.com"
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user), SessionID: "only-session",
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	fs := ui.open[f.ID]
	fs.mu.Lock()
	fs.owner = ""
	fs.mu.Unlock()
	if got, want := fs.lockOwner(ui.owner), locks.Owner(user, "only-session"); got != want {
		t.Errorf("an unstamped folder announced %q, want the index's own %q", got, want)
	}
	if fmt.Sprint(fs.lockOwner("")) != "" {
		t.Error("an unstamped folder with no fallback announced something it was never given")
	}
}
