package msgcache

import (
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func compatFolder(t *testing.T) (mailbox.UserIndex, *mailbox.Folder, *mailbox.MessageMeta) {
	t.Helper()
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "u", Home: t.TempDir()})
	f, err := idx.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{UID: 1, Filename: "m"}
	if err := idx.AppendMessage(f.ID, m); err != nil {
		t.Fatal(err)
	}
	return idx, f, m
}

// reread returns the message as the index now holds it: the cache offset is
// stamped there on Close, so a stale struct from before the write points at
// nothing and every read looks like a miss.
func reread(t *testing.T, idx mailbox.UserIndex, folderID uint64, uid uint32) *mailbox.MessageMeta {
	t.Helper()
	msgs, err := idx.GetMessages(folderID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.UID == uid {
			return m
		}
	}
	t.Fatalf("uid %d vanished from the index", uid)
	return nil
}

// A cache file written by a version that knows References must stay readable
// to one that does not.
//
// The field table lives in the file, so a reader looks its own fields up by
// name and meets the third as an id it never asks about. This asserts that on
// the bytes rather than on that reasoning: the envelope written beside the new
// field still decodes, which is what an older binary would be doing.
//
// A rollback after an upgrade is the case, and it is asked about during an
// incident, so it is answered here instead.
func TestAReaderWithoutReferencesStillReadsTheFile(t *testing.T) {
	idx, f, m := compatFolder(t)
	want := &imaplib.Envelope{Subject: "Plan", MessageID: "<a@x>", Date: time.Unix(1770000000, 0).UTC()}

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, want)
	fc.StoreReferences(m, []string{"<root@x>", "<mid@x>"})
	fc.Close()

	// What an older binary does: open the same pair, ask for the two fields it
	// knows, and read the record.
	old := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if old == nil {
		t.Fatal("cache unavailable on the second open")
	}
	defer old.Close()
	old.refsID = 1 << 30 // a field id this reader never registered

	m = reread(t, idx, f.ID, m.UID)
	got := old.Envelope(m)
	if got == nil {
		t.Fatal("the envelope became unreadable once a third field was written beside it")
	}
	if got.Subject != want.Subject || got.MessageID != want.MessageID {
		t.Errorf("envelope = %+v, want %+v", got, want)
	}
	if _, cached := old.References(m); cached {
		t.Error("a reader that never registered the field found one anyway")
	}
}

// The other direction: a file written before the field existed. The read must
// be a miss, not an error and not an empty answer -- an empty answer would
// thread every pre-existing message as if it had no ancestry.
func TestAFileWithoutReferencesIsAMissNotAnAnswer(t *testing.T) {
	idx, f, m := compatFolder(t)

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan"})
	fc.Close()

	fresh := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fresh == nil {
		t.Fatal("cache unavailable")
	}
	defer fresh.Close()
	m = reread(t, idx, f.ID, m.UID)
	if refs, cached := fresh.References(m); cached {
		t.Errorf("References reported %v as cached for a message stored before the field existed", refs)
	}
	if fresh.Envelope(m) == nil {
		t.Error("the envelope from the older writer is no longer readable")
	}
}

// "No References" is an answer and must be cached as one, or every message
// without the header is read again on every command for ever.
func TestAMessageWithNoReferencesIsCachedAsHavingNone(t *testing.T) {
	idx, f, m := compatFolder(t)

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreReferences(m, nil)
	fc.Close()

	again := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if again == nil {
		t.Fatal("cache unavailable")
	}
	defer again.Close()
	m = reread(t, idx, f.ID, m.UID)
	refs, cached := again.References(m)
	if !cached {
		t.Fatal("a message with no References reads as a miss, so it will be re-read for ever")
	}
	if len(refs) != 0 {
		t.Errorf("References = %v, want none", refs)
	}
}

// Round trip, because the ids are the thing threading joins on: a lost or
// reordered id changes which conversation a message lands in.
func TestReferencesRoundTripInOrder(t *testing.T) {
	idx, f, m := compatFolder(t)
	want := []string{"<root@x>", "<second@x>", "<third@x>"}

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreReferences(m, want)
	fc.Close()

	again := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if again == nil {
		t.Fatal("cache unavailable")
	}
	defer again.Close()
	m = reread(t, idx, f.ID, m.UID)
	got, cached := again.References(m)
	if !cached {
		t.Fatal("not cached")
	}
	if len(got) != len(want) {
		t.Fatalf("References = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("id %d = %q, want %q -- order decides the parent", i, got[i], want[i])
		}
	}
}

// The combined read must answer exactly what the two separate reads answer, in
// every state a record can be in.
//
// It replaced the pair for speed, and speed is the one reason to accept a
// second implementation of a read -- so the second one has to agree with the
// first everywhere, not only where a wire test happened to exercise it. The
// state that matters most here is the middle one: a record whose envelope is
// cached and whose References field is not. That is every message stored
// before the field existed, and it must read as NOT cached, or threading would
// treat "we have not looked" as "this message has no ancestry" and quietly put
// it in the wrong conversation.
func TestCombinedReadAgreesWithThePair(t *testing.T) {
	tests := []struct {
		name  string
		store func(fc *Handle, m *mailbox.MessageMeta)
	}{
		{
			name: "no References field at all",
			store: func(fc *Handle, m *mailbox.MessageMeta) {
				fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan", MessageID: "<a@x>"})
			},
		},
		{
			name: "References cached as empty",
			store: func(fc *Handle, m *mailbox.MessageMeta) {
				fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan", MessageID: "<a@x>"})
				fc.StoreReferences(m, nil)
			},
		},
		{
			name: "References cached with ids",
			store: func(fc *Handle, m *mailbox.MessageMeta) {
				fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan", MessageID: "<b@x>"})
				fc.StoreReferences(m, []string{"<root@x>", "<mid@x>"})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx, f, m := compatFolder(t)
			fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
			if fc == nil {
				t.Fatal("cache unavailable")
			}
			tc.store(fc, m)
			fc.Close()

			read := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
			if read == nil {
				t.Fatal("cache unavailable")
			}
			defer read.Close()
			m = reread(t, idx, f.ID, m.UID)

			wantEnv := read.Envelope(m)
			wantRefs, wantCached := read.References(m)
			gotEnv, gotRefs, gotCached := read.EnvelopeAndReferences(m)

			switch {
			case (wantEnv == nil) != (gotEnv == nil):
				t.Fatalf("envelope presence differs: pair=%v combined=%v", wantEnv != nil, gotEnv != nil)
			case wantEnv != nil && (wantEnv.Subject != gotEnv.Subject || wantEnv.MessageID != gotEnv.MessageID):
				t.Errorf("envelope differs: pair=%+v combined=%+v", wantEnv, gotEnv)
			}
			if wantCached != gotCached {
				t.Errorf("cached flag = %v, the pair says %v -- 'we have not looked' and 'it has none' are different answers",
					gotCached, wantCached)
			}
			if len(wantRefs) != len(gotRefs) {
				t.Fatalf("references = %v, the pair says %v", gotRefs, wantRefs)
			}
			for i := range wantRefs {
				if wantRefs[i] != gotRefs[i] {
					t.Errorf("reference %d = %q, the pair says %q", i, gotRefs[i], wantRefs[i])
				}
			}
		})
	}
}

// A snapshot must never answer with bytes that are no longer the truth.
//
// Preload reads the file as it stands; the same handle then keeps appending,
// because a scan writes back what it had to parse. Anything appended lives
// past the snapshot's end and has to come from the file — otherwise a message
// cached during the scan would read as a miss for the rest of it, or worse,
// resolve to whatever occupied that offset earlier.
func TestAPreloadedHandleStillSeesWhatItAppends(t *testing.T) {
	idx, f, first := compatFolder(t)

	// One message already cached, so the snapshot has something in it.
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(first, &imaplib.Envelope{Subject: "First", MessageID: "<a@x>"})
	fc.Close()

	second := &mailbox.MessageMeta{UID: 2, Filename: "m"}
	if err := idx.AppendMessage(f.ID, second); err != nil {
		t.Fatal(err)
	}

	read := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if read == nil {
		t.Fatal("cache unavailable")
	}
	read.Preload() // snapshot taken here: it contains the first message only

	// What the scan does on a miss: parse, store, and expect to read it back.
	read.StoreEnvelope(second, &imaplib.Envelope{Subject: "Second", MessageID: "<b@x>"})
	if env := read.Envelope(second); env == nil {
		t.Fatal("a message cached after the snapshot reads as a miss -- the snapshot is answering for offsets it does not hold")
	} else if env.Subject != "Second" {
		t.Errorf("subject = %q, want \"Second\" -- the read resolved inside the snapshot rather than in the file", env.Subject)
	}

	// And the message that was in the snapshot still reads correctly.
	if env := read.Envelope(reread(t, idx, f.ID, first.UID)); env == nil || env.Subject != "First" {
		t.Errorf("the pre-existing message no longer reads back: %+v", env)
	}
	read.Close()
}
