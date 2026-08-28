package msgcache

import (
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func newFolder(t *testing.T) (mailbox.UserIndex, uint64, *mailbox.MessageMeta) {
	t.Helper()
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "u", Home: t.TempDir()})
	f, err := idx.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{UID: 1}
	if err := idx.AppendMessage(f.ID, m); err != nil {
		t.Fatal(err)
	}
	return idx, f.ID, m
}

// A deferred handle does not hold the cache locks while its caller writes a
// response.
//
// This is the whole point of #1545: FETCH read bodies from storage and wrote
// them to a socket inside the locked window, so a client on a slow link held
// both tiers and every other session of that user on that folder waited.
//
// Opening a second handle is what a second session does. With the window held
// it blocks until the first one closes; the deadline below is what tells the
// two apart, and with DeferWrites removed this test fails on it rather than
// hanging for ever.
func TestADeferredHandleDoesNotHoldTheLocksWhileTheCallerWorks(t *testing.T) {
	idx, fid, m := newFolder(t)

	first := Open(idx, fid, Options{DeferWrites: true})
	if first == nil {
		t.Fatal("no cache handle")
	}
	defer first.Close()
	first.StoreEnvelope(m, &imaplib.Envelope{Subject: "one"})

	opened := make(chan *Handle, 1)
	go func() { opened <- Open(idx, fid, Options{}) }()

	select {
	case second := <-opened:
		if second == nil {
			t.Fatal("the second handle could not open the pair")
		}
		second.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("the second session waited on a handle that is only writing later: the locks are still held across the caller's work")
	}
}

// What the request parsed is still cached, in a second window.
//
// Deferring must not become discarding: the first fetch parses and the second
// must not have to.
func TestWhatADeferredHandleCollectedIsWrittenOnClose(t *testing.T) {
	idx, fid, m := newFolder(t)

	h := Open(idx, fid, Options{DeferWrites: true})
	if h == nil {
		t.Fatal("no cache handle")
	}
	h.StoreEnvelope(m, &imaplib.Envelope{Subject: "kept"})
	h.Close()

	// A fresh read, through the index the stamp was written to.
	msgs, err := idx.GetMessages(fid, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	back := Open(idx, fid, Options{})
	if back == nil {
		t.Fatal("no cache handle for the read")
	}
	defer back.Close()
	env := back.Envelope(msgs[0])
	if env == nil {
		t.Fatal("nothing was cached, so every later fetch re-parses")
	}
	if env.Subject != "kept" {
		t.Errorf("cached subject is %q, want %q", env.Subject, "kept")
	}
}

// A generation bumped between the two windows drops the writes.
//
// The offsets a deferred handle computed belong to the file it read. Appending
// them into a new generation would stamp the index at positions where another
// message's record lands later -- a valid record for the wrong message, which
// no invalidation level can detect. Dropping them costs a re-parse.
func TestADeferredHandleDropsItsWritesWhenTheGenerationMoved(t *testing.T) {
	idx, fid, m := newFolder(t)

	h := Open(idx, fid, Options{DeferWrites: true})
	if h == nil {
		t.Fatal("no cache handle")
	}
	h.StoreEnvelope(m, &imaplib.Envelope{Subject: "stale"})

	// Somebody else found the pair invalid and started a new generation.
	ic, ok := idx.(Index)
	if !ok {
		t.Skip("index has no cache surface")
	}
	if _, err := ic.BumpCacheGeneration(fid); err != nil {
		t.Fatalf("bump: %v", err)
	}

	h.Close()

	msgs, err := idx.GetMessages(fid, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	back := Open(idx, fid, Options{})
	if back == nil {
		t.Fatal("no cache handle for the read")
	}
	defer back.Close()
	if env := back.Envelope(msgs[0]); env != nil {
		t.Errorf("a record written against the old generation was kept: %q", env.Subject)
	}
}

// A record another session wrote between the two windows is not hopped over.
//
// The deferred handle carries the chain head it saw when the request started.
// If another session appends for the same UID and stamps a new head in the
// meantime, chaining from the old offset writes a record whose next pointer
// skips theirs, and theirs becomes unreachable -- a silent cache loss, and the
// exact thing splitting the window has to tolerate rather than cause.
//
// Two different fields on one message, so the assertion is which of them
// survive: the second window's own value is trivially there, and the other
// one is what the chain either keeps or loses.
func TestADeferredHandleDoesNotHopOverAnotherSessionsRecord(t *testing.T) {
	idx, fid, m := newFolder(t)

	first := Open(idx, fid, Options{DeferWrites: true})
	if first == nil {
		t.Fatal("no cache handle")
	}
	first.StoreEnvelope(m, &imaplib.Envelope{Subject: "mine"})

	// Another session, in the gap: same message, a different field.
	other := Open(idx, fid, Options{})
	if other == nil {
		t.Fatal("no second handle")
	}
	msgs, err := idx.GetMessages(fid, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	other.StoreReferences(msgs[0], []string{"<ref@example.com>"})
	other.Close()

	first.Close()

	msgs, err = idx.GetMessages(fid, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	back := Open(idx, fid, Options{})
	if back == nil {
		t.Fatal("no handle for the read")
	}
	defer back.Close()

	if env := back.Envelope(msgs[0]); env == nil || env.Subject != "mine" {
		t.Errorf("the deferred handle's own field is missing: %+v", env)
	}
	if refs, ok := back.References(msgs[0]); !ok || len(refs) != 1 {
		t.Errorf("the other session's record was hopped over and is unreachable: refs=%v ok=%v", refs, ok)
	}
}
