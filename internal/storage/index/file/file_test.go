package file

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestLogReplay(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	f, _ := b.OpenFolder("u@x.com", "INBOX", 42)

	// Append 3 messages, flag-update one, expunge one.
	for i := uint32(1); i <= 3; i++ {
		modseq, _ := b.NextModSeq(f.ID)
		b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: i, Flags: []string{`\Seen`}, ModSeq: modseq}) //nolint:errcheck
	}
	b.UpdateFlags(f.ID, 2, []string{`\Seen`, `\Flagged`}, nil) //nolint:errcheck
	b.ExpungeMessage(f.ID, 3)                                   //nolint:errcheck
	b.Close()                                                    //nolint:errcheck

	// Reopen — all state must come from replaying .index.log.
	b2 := New(dir)
	f2, err := b2.OpenFolder("u@x.com", "INBOX", 42)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	msgs, err := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("after log replay: got %d messages, want 2", len(msgs))
	}
	// UID 2 must have \Flagged
	var found bool
	for _, m := range msgs {
		if m.UID == 2 {
			found = true
			hasFlagged := false
			for _, fl := range m.Flags {
				if fl == `\Flagged` {
					hasFlagged = true
				}
			}
			if !hasFlagged {
				t.Errorf("UID 2: expected \\Flagged in %v", m.Flags)
			}
		}
		if m.UID == 3 {
			t.Error("expunged UID 3 still present after log replay")
		}
	}
	if !found {
		t.Error("UID 2 missing after log replay")
	}
	b2.Close() //nolint:errcheck
}

func TestLogReplay_Keywords(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	f, _ := b.OpenFolder("u@x.com", "INBOX", 1)

	modseq, _ := b.NextModSeq(f.ID)
	b.AppendMessage(f.ID, &mailbox.MessageMeta{ //nolint:errcheck
		UID:      1,
		Flags:    []string{`\Seen`},
		Keywords: []string{"$Forwarded"},
		ModSeq:   modseq,
	})
	b.Close() //nolint:errcheck

	b2 := New(dir)
	f2, _ := b2.OpenFolder("u@x.com", "INBOX", 1)
	msgs, _ := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if len(msgs) != 1 {
		t.Fatalf("after keyword log replay: got %d messages, want 1", len(msgs))
	}
	found := false
	for _, kw := range msgs[0].Keywords {
		if kw == "$Forwarded" {
			found = true
		}
	}
	if !found {
		t.Errorf("$Forwarded missing after log replay: %v", msgs[0].Keywords)
	}
	b2.Close() //nolint:errcheck
}

func TestOpenFolder_CreateAndReopen(t *testing.T) {
	b := New(t.TempDir())

	f, err := b.OpenFolder("alice@x.com", "INBOX", 12345)
	if err != nil {
		t.Fatal(err)
	}
	if f.UIDValidity != 12345 {
		t.Errorf("UIDValidity = %d, want 12345", f.UIDValidity)
	}
	if f.NextUID != 1 {
		t.Errorf("NextUID = %d, want 1", f.NextUID)
	}
	b.Close() //nolint:errcheck

	// Reopen — must restore header from disk.
	b2 := New(b.root)
	f2, err := b2.OpenFolder("alice@x.com", "INBOX", 12345)
	if err != nil {
		t.Fatal(err)
	}
	if f2.UIDValidity != f.UIDValidity {
		t.Errorf("reopened UIDValidity = %d, want %d", f2.UIDValidity, f.UIDValidity)
	}
	b2.Close() //nolint:errcheck
}

func TestAppendAndGetMessages(t *testing.T) {
	b := New(t.TempDir())
	f, _ := b.OpenFolder("u@x.com", "INBOX", 1)

	for i := uint32(1); i <= 5; i++ {
		modseq, _ := b.NextModSeq(f.ID)
		m := &mailbox.MessageMeta{UID: i, Flags: []string{`\Seen`}, ModSeq: modseq}
		if err := b.AppendMessage(f.ID, m); err != nil {
			t.Fatalf("AppendMessage uid=%d: %v", i, err)
		}
	}

	msgs, err := b.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5", len(msgs))
	}
	b.Close() //nolint:errcheck
}

func TestUpdateFlags(t *testing.T) {
	b := New(t.TempDir())
	f, _ := b.OpenFolder("u@x.com", "INBOX", 1)

	modseq, _ := b.NextModSeq(f.ID)
	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Flags: []string{`\Seen`}, ModSeq: modseq}) //nolint:errcheck

	if err := b.UpdateFlags(f.ID, 1, []string{`\Seen`, `\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 1}})
	if len(msgs) == 0 {
		t.Fatal("message not found after UpdateFlags")
	}
	hasFlag := func(flags []string, f string) bool {
		for _, fl := range flags {
			if fl == f {
				return true
			}
		}
		return false
	}
	if !hasFlag(msgs[0].Flags, `\Flagged`) {
		t.Errorf("expected \\Flagged in %v", msgs[0].Flags)
	}
	b.Close() //nolint:errcheck
}

func TestExpungeMessage(t *testing.T) {
	b := New(t.TempDir())
	f, _ := b.OpenFolder("u@x.com", "INBOX", 1)

	for i := uint32(1); i <= 3; i++ {
		b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: i, Flags: []string{`\Deleted`}}) //nolint:errcheck
	}
	if err := b.ExpungeMessage(f.ID, 2); err != nil {
		t.Fatal(err)
	}
	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	if len(msgs) != 2 {
		t.Fatalf("after expunge: got %d messages, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m.UID == 2 {
			t.Error("expunged UID 2 still present")
		}
	}
	b.Close() //nolint:errcheck
}

func TestSeqSetContains(t *testing.T) {
	cases := []struct {
		s    mailbox.SeqSet
		uid  uint32
		want bool
	}{
		{mailbox.SeqSet{}, 99, true}, // empty = all
		{mailbox.SeqSet{{From: 1, To: 5}}, 3, true},
		{mailbox.SeqSet{{From: 1, To: 5}}, 6, false},
		{mailbox.SeqSet{{From: 10, To: 0}}, 999999, true}, // To=0 means *
		{mailbox.SeqSet{{From: 1, To: 3}, {From: 7, To: 9}}, 8, true},
		{mailbox.SeqSet{{From: 1, To: 3}, {From: 7, To: 9}}, 5, false},
	}
	for _, tc := range cases {
		got := seqSetContains(tc.s, tc.uid)
		if got != tc.want {
			t.Errorf("seqSetContains(%v, %d) = %v, want %v", tc.s, tc.uid, got, tc.want)
		}
	}
}

func TestKeywordsRoundTrip(t *testing.T) {
	b := New(t.TempDir())
	f, _ := b.OpenFolder("u@x.com", "INBOX", 1)

	modseq, _ := b.NextModSeq(f.ID)
	err := b.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID:      1,
		Flags:    []string{`\Seen`},
		Keywords: []string{"$Forwarded", "$Junk"},
		ModSeq:   modseq,
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	hasKW := func(kws []string, k string) bool {
		for _, kw := range kws {
			if kw == k {
				return true
			}
		}
		return false
	}
	if !hasKW(msgs[0].Keywords, "$Forwarded") {
		t.Errorf("$Forwarded not in keywords: %v", msgs[0].Keywords)
	}
	if !hasKW(msgs[0].Keywords, "$Junk") {
		t.Errorf("$Junk not in keywords: %v", msgs[0].Keywords)
	}
	b.Close() //nolint:errcheck

	// Verify keywords survive a close+reopen (disk persistence).
	b2 := New(b.root)
	f2, _ := b2.OpenFolder("u@x.com", "INBOX", 1)
	msgs2, _ := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if len(msgs2) != 1 {
		t.Fatalf("after reopen: got %d messages, want 1", len(msgs2))
	}
	if !hasKW(msgs2[0].Keywords, "$Forwarded") {
		t.Errorf("after reopen: $Forwarded not in keywords: %v", msgs2[0].Keywords)
	}
	b2.Close() //nolint:errcheck
}

func TestKeywordsUpdateFlags(t *testing.T) {
	b := New(t.TempDir())
	f, _ := b.OpenFolder("u@x.com", "INBOX", 1)

	b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Flags: []string{`\Seen`}}) //nolint:errcheck

	if err := b.UpdateFlags(f.ID, 1, []string{`\Seen`}, []string{"$NotJunk"}); err != nil {
		t.Fatal(err)
	}

	msgs, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	if len(msgs) == 0 {
		t.Fatal("no messages after UpdateFlags")
	}
	found := false
	for _, kw := range msgs[0].Keywords {
		if kw == "$NotJunk" {
			found = true
		}
	}
	if !found {
		t.Errorf("$NotJunk not in keywords after UpdateFlags: %v", msgs[0].Keywords)
	}
	b.Close() //nolint:errcheck
}

func TestFlagConversion(t *testing.T) {
	flags := []string{`\Answered`, `\Flagged`, `\Deleted`, `\Seen`, `\Draft`}
	idx := imapFlagsToIndex(flags)
	back := indexFlagsToIMAP(idx)

	has := func(sl []string, f string) bool {
		for _, s := range sl {
			if s == f {
				return true
			}
		}
		return false
	}
	for _, f := range flags {
		if !has(back, f) {
			t.Errorf("flag %q lost in conversion (index byte: %08b)", f, idx)
		}
	}
}
