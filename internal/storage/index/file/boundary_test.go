package file

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestBoundaryPartialWriteDiscarded verifies that a partial write appended
// after the last complete BOUNDARY is dropped on re-open.
func TestBoundaryPartialWriteDiscarded(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 0)

	modseq, _ := b.NextModSeq(f.ID)
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, ModSeq: modseq, Filename: "a.eml", Size: 100}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	b.Close() //nolint:errcheck

	// Corrupt: append a torn TxHeader (8 bytes claiming size=16, no payload, no BOUNDARY).
	logPath := filepath.Join(dir, testHome("", testUser), "INBOX", IndexLogFileName)
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	torn := make([]byte, 8)
	binary.LittleEndian.PutUint32(torn[0:], 16) // size=16 (8-byte payload never written)
	binary.LittleEndian.PutUint32(torn[4:], 2)  // TxTypeAppend
	_, _ = lf.Write(torn)
	lf.Close()

	// Reopen — torn record must be discarded; original message must survive.
	b2 := openIdx(dir, testUser)
	f2, _ := b2.OpenFolder("INBOX", 0)
	msgs, err := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("GetMessages after corruption: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message after partial-write recovery, got %d", len(msgs))
	}
	if msgs[0].UID != 1 {
		t.Fatalf("want UID=1, got %d", msgs[0].UID)
	}
}

// TestBoundaryTwoCompleteGroups verifies that two consecutive complete
// BOUNDARY groups are both applied on replay.
func TestBoundaryTwoCompleteGroups(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser2)
	f, _ := b.OpenFolder("INBOX", 0)

	for i, name := range []string{"a.eml", "b.eml"} {
		modseq, _ := b.NextModSeq(f.ID)
		if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uint32(i + 1), ModSeq: modseq, Filename: name, Size: 100,
		}); err != nil {
			t.Fatalf("AppendMessage %s: %v", name, err)
		}
	}
	b.Close() //nolint:errcheck

	b2 := openIdx(dir, testUser2)
	f2, _ := b2.OpenFolder("INBOX", 0)
	msgs, _ := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
}
