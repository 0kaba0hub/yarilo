package file

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

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
		{mailbox.SeqSet{}, 99, true},                                   // empty = all
		{mailbox.SeqSet{{From: 1, To: 5}}, 3, true},
		{mailbox.SeqSet{{From: 1, To: 5}}, 6, false},
		{mailbox.SeqSet{{From: 10, To: 0}}, 999999, true},             // To=0 means *
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
