package file

import (
	"os"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestApplyLogConfirmedEndExcludesTornIncrementalTail is a regression test
// for #667: applyLog's return value is the only thing callers may use to
// advance fs.logSize — never an external os.Stat taken before or after the
// call, since a concurrent writer's append can land mid-read and desync the
// two. This pins the incremental (fromOffset>0) path specifically: a torn
// trailing group past fromOffset (simulating a writer's append that was
// only partially visible to this reader's read pass) must NOT be counted as
// confirmed, even though the incremental path never truncates the file (a
// legitimate in-flight writer may still be appending at that exact spot —
// see applyLog's no-truncate rationale). The returned offset must stay at
// fromOffset so the next reload() naturally retries from a safe point.
func TestApplyLogConfirmedEndExcludesTornIncrementalTail(t *testing.T) {
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
	baseline := fs.logSize // confirmed position after the clean append above

	logPath := fs.indexPath + ".log"
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log for garbage append: %v", err)
	}
	// Simulate a writer's append that landed only partially in this reader's
	// view: a plausible-looking tx header whose declared payload never
	// arrives complete.
	if _, err := lf.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02}); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := lf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fs.mu.Lock()
	confirmedEnd, applyErr := fs.applyLog(baseline)
	fs.mu.Unlock()
	if applyErr != nil {
		t.Fatalf("applyLog: %v", applyErr)
	}
	if confirmedEnd != baseline {
		t.Errorf("confirmedEnd advanced past a torn incremental tail: got %d, want %d (baseline)", confirmedEnd, baseline)
	}

	// The incremental path must never truncate — a legitimate writer could
	// still be mid-append at that exact position.
	st, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if st.Size() == baseline {
		t.Error("incremental applyLog truncated the log — it must only skip counting the torn tail as confirmed, never remove bytes")
	}
}
