package jmap

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The keying, asserted where it actually decides. A cache keyed on the folder
// name alone would answer one account with another's marker, because every
// account has an INBOX.
//
// This is a unit row rather than a whole-server one on purpose: at the server
// level two accounts have different files, so their stamps differ and the entry
// misses anyway -- the integration test below would pass with a global key and
// prove nothing. Here the stamp is held equal, which is the only way to ask
// whether the key carries the account.
func TestStateCacheKeysOnTheAccount(t *testing.T) {
	c := newStateCache()
	stamp := mailbox.FolderStamp{BaseSize: 100, LogSize: 40}
	mine := cachedMark{stamp: stamp, key: [8]byte{1}, fields: []uint64{1, 2, 3}}
	c.put("u1@example.com", "INBOX", mine)

	if _, ok := c.get("u2@example.com", "INBOX", stamp); ok {
		t.Error("another account's INBOX marker was served under an identical stamp")
	}
	if got, ok := c.get("u1@example.com", "INBOX", stamp); !ok || got.key != mine.key {
		t.Error("the owning account did not get its own marker back")
	}
}

// And end to end, as a sanity check: two accounts must not report the same
// state. This one passes on a global key too -- see above -- so it stands as
// defence in depth rather than as the proof.
func TestStateCacheDoesNotCrossAccounts(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)

	// A second account over the same server, with its own store.
	otherHome := t.TempDir()
	otherUser := "u2@example.com"
	base := s.opts.Storage.ResolveUser
	s.opts.Storage.ResolveUser = func(name string) (*mailbox.UserInfo, error) {
		if name == otherUser {
			return &mailbox.UserInfo{Username: otherUser, Home: otherHome, Separator: "/"}, nil
		}
		return base(name)
	}

	first, err := s.opts.Storage.open(testUser)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer first.close()
	firstState, err := s.emailState(first)
	if err != nil {
		t.Fatalf("state of the first account: %v", err)
	}

	second, err := s.opts.Storage.open(otherUser)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer second.close()
	if err := second.box.Init(); err != nil {
		t.Fatalf("init second: %v", err)
	}
	if _, err := second.idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open second INBOX: %v", err)
	}
	secondState, err := s.emailState(second)
	if err != nil {
		t.Fatalf("state of the second account: %v", err)
	}

	if firstState == secondState {
		t.Error("two accounts report the same state; a marker was served across accounts")
	}
}

// The shrink direction, asserted where it is observable at all.
//
// A fold changes no field of the marker -- uidvalidity, modseq and nextUID are
// all fold-invariant -- so a stale entry and a freshly computed one produce the
// identical state string. Comparing states therefore cannot tell them apart,
// and an integration test that does so passes even on a cache that only
// invalidates on growth. The difference is observable only as hit against
// miss, which is what this row asks.
func TestStateCacheInvalidatesOnAShrunkLog(t *testing.T) {
	c := newStateCache()
	beforeFold := mailbox.FolderStamp{BaseSize: 4096, LogSize: 6600}
	c.put(testUser, "INBOX", cachedMark{stamp: beforeFold, key: [8]byte{7}, fields: []uint64{1, 2, 3}})

	// What the same folder looks like after a fold: the base was rewritten and
	// the log is a header again.
	afterFold := mailbox.FolderStamp{BaseSize: 4096, LogSize: 40}
	if _, ok := c.get(testUser, "INBOX", afterFold); ok {
		t.Error("a marker from before the fold was served after it; the cache is watching only growth")
	}
}

// The log shrinks when a fold truncates it, and a cache that only noticed
// growth would keep serving a marker from before the fold. Both directions
// invalidate, which is why the proof is the pair (size, mtime) of both files
// rather than a length that only ever increases.
func TestStateCacheInvalidatesWhenTheLogShrinks(t *testing.T) {
	s, _, _ := storedServerWithMessageAt(t, setTestMessage, 0)

	warm, err := s.opts.Storage.open(testUser)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	before, err := s.emailState(warm)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	marks, err := s.folderMarks(warm)
	if err != nil || len(marks) == 0 {
		t.Fatalf("folder marks: %v %v", marks, err)
	}
	// A change, so the log carries something, then the fold that takes it away.
	if _, err := warm.idx.UpdateFlagsMulti(marks[0].folder.ID, map[uint32]mailbox.FlagsUpdate{
		1: {Mode: mailbox.FlagsAdd, Keywords: []string{"$cachetest"}},
	}); err != nil {
		t.Fatalf("store: %v", err)
	}
	changed, err := s.emailState(warm)
	if err != nil {
		t.Fatalf("state after the store: %v", err)
	}
	if changed == before {
		t.Fatal("the state did not move after a write; the cache served a stale marker")
	}
	if err := warm.idx.OptimizeIndex(marks[0].folder.ID); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	warm.close()

	// A fresh handle, as a later request gets. The fold rewrote the base and
	// truncated the log: the marker must be recomputed, not served.
	after, err := s.opts.Storage.open(testUser)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer after.close()
	folded, err := s.emailState(after)
	if err != nil {
		t.Fatalf("state after the fold: %v", err)
	}
	// The fold changes no message, so the marker's values are the same as
	// before it; what must not happen is a state built from the pre-fold entry
	// without checking. That is asserted by rebuilding from a cold read and
	// comparing.
	cold, err := s.opts.Storage.open(testUser)
	if err != nil {
		t.Fatalf("cold open: %v", err)
	}
	defer cold.close()
	coldMarks, err := s.folderMarks(cold)
	if err != nil {
		t.Fatalf("cold marks: %v", err)
	}
	if folded != coldMarks.description().String() {
		t.Errorf("state after the fold = %q, a cold read says %q -- the cache was not invalidated",
			folded, coldMarks.description().String())
	}
}
