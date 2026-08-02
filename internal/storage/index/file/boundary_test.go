package file

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestBoundaryPartialWriteDiscarded verifies that a partial write appended
// after the last complete BOUNDARY is dropped on re-open.
func TestBoundaryPartialWriteDiscarded(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 0, "")

	modseq, _ := b.NextModSeq(f.ID)
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, ModSeq: modseq, Filename: "a.eml", Size: 100}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	b.Close() //nolint:errcheck

	// Corrupt: append a torn TxHeader (8 bytes claiming size=16, no payload, no BOUNDARY).
	logPath := filepath.Join(dir, testHome("", testUser), IndexLogFileName)
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
	f2, _ := b2.OpenFolder("INBOX", 0, "")
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

// TestBoundaryMidTransactionCrashDiscarded verifies that a BOUNDARY record
// whose declared size extends beyond the actual file end causes the entire
// transaction to be discarded — no sub-records are applied.
func TestBoundaryMidTransactionCrashDiscarded(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, _ := b.OpenFolder("INBOX", 0, "")

	// Write one good message so we have a baseline.
	modseq, _ := b.NextModSeq(f.ID)
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, ModSeq: modseq, Filename: "a.eml", Size: 100}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	b.Close() //nolint:errcheck

	// Craft a torn transaction: BOUNDARY (12 bytes) + only 4 bytes of a
	// sub-record that would require at least 8 bytes (header) + payload.
	// BOUNDARY.Size = 12 + 24 = 36, but we only write 4 bytes after the
	// BOUNDARY, so logFileSize < committedEnd — transaction must be dropped.
	logPath := filepath.Join(dir, testHome("", testUser), IndexLogFileName)
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	// BOUNDARY record: size=12 (8 hdr + 4 payload), type=TxTypeBoundary(0x00080000)
	// payload = uint32(36) — claims 36 bytes total in this transaction group.
	boundary := make([]byte, 12)
	binary.LittleEndian.PutUint32(boundary[0:], 12)         // TxHeader.Size
	binary.LittleEndian.PutUint32(boundary[4:], 0x00080000) // TxTypeBoundary
	binary.LittleEndian.PutUint32(boundary[8:], 36)         // payload: total group size
	if _, err := lf.Write(boundary); err != nil {
		t.Fatalf("write boundary: %v", err)
	}
	// Write only 4 bytes of a sub-record (torn mid-header).
	partial := make([]byte, 4)
	binary.LittleEndian.PutUint32(partial[0:], 24) // claims size=24 but no more bytes follow
	if _, err := lf.Write(partial); err != nil {
		t.Fatalf("write partial sub-record: %v", err)
	}
	lf.Close()

	// Reopen — torn transaction must be discarded; only the original message survives.
	b2 := openIdx(dir, testUser)
	f2, _ := b2.OpenFolder("INBOX", 0, "")
	msgs, err := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("GetMessages after mid-transaction crash: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message after mid-transaction crash recovery, got %d", len(msgs))
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
	f, _ := b.OpenFolder("INBOX", 0, "")

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
	f2, _ := b2.OpenFolder("INBOX", 0, "")
	msgs, _ := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
}
