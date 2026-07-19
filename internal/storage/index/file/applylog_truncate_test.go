package file

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestApplyLogTruncatesGenuineTornTail is a regression test for applyLog's
// partial-tail cleanup: a log whose final bytes are not a parseable record
// (simulating a crash mid-write) must still be truncated back to the last
// complete BOUNDARY, exactly as before the filePos-based rewrite.
func TestApplyLogTruncatesGenuineTornTail(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	uid, err := b.AllocateUID(f.ID)
	if err != nil {
		t.Fatalf("AllocateUID: %v", err)
	}
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "1.eml", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	fs := b.open[f.ID]
	logPath := fs.indexPath + ".log"
	clean, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat clean log: %v", err)
	}
	cleanSize := clean.Size()

	// Append garbage bytes simulating a torn write past the last good boundary.
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log for garbage append: %v", err)
	}
	if _, err := lf.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02}); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := lf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fs.mu.Lock()
	fs.file, err = mailindex.Open(fs.indexPath)
	if err != nil {
		fs.mu.Unlock()
		t.Fatalf("reopen base: %v", err)
	}
	applyErr := fs.applyLog(0)
	fs.mu.Unlock()
	if applyErr != nil {
		t.Fatalf("applyLog: %v", applyErr)
	}

	after, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Size() != cleanSize {
		t.Errorf("torn tail not truncated: got size %d, want %d (clean)", after.Size(), cleanSize)
	}
}

// TestConcurrentReadersDoNotTruncateLiveAppends reproduces the applyLog
// TOCTOU this test file's sibling fix addresses: applyLog(0) runs unlocked
// via readBase on every OpenFolder of an already-existing folder (the common
// case — a new IMAP connection SELECTing an established mailbox). Before the
// fix, applyLog's tail cleanup compared its own read against a SEPARATE,
// later os.Stat; a concurrent writer's fully-valid, already-committed append
// landing in the gap between the read finishing and that stat would be
// mistaken for a crash-torn tail and truncated away — silently destroying
// the writer's data. This drives a long-lived writer appending many messages
// against a flood of short-lived readers repeatedly opening the same folder
// throughout, and asserts every appended message survives.
func TestConcurrentReadersDoNotTruncateLiveAppends(t *testing.T) {
	dir := t.TempDir()

	writer := openIdx(dir, testUser)
	wf, err := writer.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("writer OpenFolder: %v", err)
	}

	const messages = 60
	const readers = 30

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < messages; i++ {
			uid, err := writer.AllocateUID(wf.ID)
			if err != nil {
				t.Errorf("AllocateUID: %v", err)
				return
			}
			if err := writer.AppendMessage(wf.ID, &mailbox.MessageMeta{
				UID: uid, Filename: "m.eml", Size: 10,
			}); err != nil {
				t.Errorf("AppendMessage uid=%d: %v", uid, err)
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
		close(stop)
	}()

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				reader := openIdx(dir, testUser)
				rf, err := reader.OpenFolder("INBOX", 1, "")
				if err != nil {
					t.Errorf("reader OpenFolder: %v", err)
					return
				}
				_, _ = reader.GetMessages(rf.ID, mailbox.SeqSet{})
			}
		}()
	}

	wg.Wait()

	final := openIdx(dir, testUser)
	ff, err := final.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("final OpenFolder: %v", err)
	}
	msgs, err := final.GetMessages(ff.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("final GetMessages: %v", err)
	}
	if len(msgs) != messages {
		t.Fatalf("message loss: got %d messages, want %d (some appends were truncated away)", len(msgs), messages)
	}
}
