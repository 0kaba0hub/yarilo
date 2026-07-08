package file

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestFsckIndexIDMismatch verifies that when the .log file carries an IndexID
// that differs from the base .index, OpenFolder detects the mismatch, truncates
// the stale log to an empty stub, and returns a clean folder state.
func TestFsckIndexIDMismatch(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "a.eml", Size: 100}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// Compact the log into the base .index so the message survives log truncation.
	if err := b.OptimizeIndex(f.ID); err != nil {
		t.Fatalf("OptimizeIndex: %v", err)
	}
	b.Close() //nolint:errcheck

	// Overwrite the .log file with a header that has a different IndexID.
	logPath := filepath.Join(dir, testHome("", testUser), IndexLogFileName)
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open log for overwrite: %v", err)
	}
	wrongID := uint32(0xDEADBEEF)
	hdr := mailindex.NewLogHeader(wrongID, 1, 0)
	if err := hdr.Encode(lf); err != nil {
		t.Fatalf("encode wrong log header: %v", err)
	}
	lf.Close()

	// Reopen — OpenFolder must detect the mismatch and recover without error.
	b2 := openIdx(dir, testUser)
	f2, err := b2.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("reopen after IndexID mismatch: %v", err)
	}

	// The message written before the mismatch is in the base .index and must
	// still be visible (log replay is skipped, not the base file).
	msgs, err := b2.GetMessages(f2.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].UID != 1 {
		t.Fatalf("want 1 message UID=1 after IndexID recovery, got %v", msgs)
	}

	// The log must have been reset to a stub with the correct IndexID.
	lf2, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open recovered log: %v", err)
	}
	defer lf2.Close()
	recoveredHdr, err := mailindex.DecodeLogHeader(lf2)
	if err != nil {
		t.Fatalf("decode recovered log header: %v", err)
	}
	if recoveredHdr.IndexID == wrongID {
		t.Fatalf("log IndexID still wrong after recovery: got %d", recoveredHdr.IndexID)
	}

	// Log must be truncated to the stub header only — no records.
	st, _ := lf2.Stat()
	if st.Size() != mailindex.LogHeaderSize {
		t.Fatalf("expected log stub size %d, got %d", mailindex.LogHeaderSize, st.Size())
	}

	b2.Close() //nolint:errcheck
}

// TestFsckCorruptedLogBeforeBoundary verifies that a corrupted tx record that
// appears before the first BOUNDARY causes the tail to be truncated to the
// log stub (LogHeaderSize bytes). This is the committedEnd=LogHeaderSize
// initialisation path added in this fix.
func TestFsckCorruptedLogBeforeBoundary(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	if _, err := b.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("initial open: %v", err)
	}
	b.Close() //nolint:errcheck

	// Append a raw corrupted record (not a valid framed size) directly
	// after the log header — no BOUNDARY precedes it.
	logPath := filepath.Join(dir, testHome("", testUser), IndexLogFileName)
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	// Write 8 bytes with a raw (non-framed) size field — DecodeFramedSize will
	// return 0, DecodeTxHeader will return ErrCorrupted, loop will break.
	garbage := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	if _, err := lf.Write(garbage); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	lf.Close()

	stBefore, _ := os.Stat(logPath)
	if stBefore.Size() <= mailindex.LogHeaderSize {
		t.Fatal("garbage was not appended")
	}

	// Reopen — the corrupted tail must be truncated back to the stub.
	b2 := openIdx(dir, testUser)
	if _, err := b2.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("reopen after corrupted record: %v", err)
	}
	b2.Close() //nolint:errcheck

	stAfter, _ := os.Stat(logPath)
	if stAfter.Size() != int64(mailindex.LogHeaderSize) {
		t.Fatalf("log not truncated: want %d bytes, got %d", mailindex.LogHeaderSize, stAfter.Size())
	}
}
