package file

import (
	"os"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// Identity, size, header and body must come from one descriptor. Reading the
// header by path and the body by another open leaves a window a sibling's
// compaction fits through: the pairing then describes one file while the replay
// reads another, and the offset it starts at means nothing in the file being
// read. Under the lock that window did not exist; a lock-free reader has to
// close it by construction.
//
// The row replaces the log after the reader has it open. What the reader holds
// must keep describing the file it will read — not the one now at the path.
func TestLogReaderDescribesTheFileItWillRead(t *testing.T) {
	dir := t.TempDir()
	indexPath := dir + "/yarilo.index"

	writeLog := func(indexID, lineage uint32, pad int) {
		t.Helper()
		f, err := os.OpenFile(indexPath+".log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		hdr := mailindex.NewLogHeader(indexID, lineage, 1)
		if err := hdr.Encode(f); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if pad > 0 {
			if _, err := f.Write(make([]byte, pad)); err != nil {
				t.Fatalf("pad: %v", err)
			}
		}
		_ = f.Close()
	}

	writeLog(7, 3, 64)
	lg, err := openLogRead(indexPath)
	if err != nil {
		t.Fatalf("openLogRead: %v", err)
	}
	defer lg.close()
	if lg.lineage() != 3 {
		t.Fatalf("lineage = %d, want 3", lg.lineage())
	}
	sizeBefore := lg.size

	// A sibling compacts: the log at the path is now a different file, with a
	// different lineage and a different length.
	if err := os.Remove(indexPath + ".log"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeLog(7, 4, 4096)

	if got := lg.lineage(); got != 3 {
		t.Errorf("the reader now reports lineage %d — it is describing a file it will not read", got)
	}
	if lg.size != sizeBefore {
		t.Errorf("the reader now reports size %d, was %d", lg.size, sizeBefore)
	}

	// And the replacement is visible to the next refresh, which is what makes
	// the previous assertion safe rather than merely stable: a stale reader is
	// only acceptable because a new one sees the new file.
	lg2, err := openLogRead(indexPath)
	if err != nil {
		t.Fatalf("openLogRead again: %v", err)
	}
	defer lg2.close()
	if lg2.lineage() != 4 {
		t.Errorf("a fresh reader reports lineage %d, want 4", lg2.lineage())
	}
}

// An absent log is a normal state — a folder whose base was just written has
// none until the next append — and must not be an error or a torn read.
func TestLogReaderOnAnAbsentLog(t *testing.T) {
	lg, err := openLogRead(t.TempDir() + "/yarilo.index")
	if err != nil {
		t.Fatalf("openLogRead: %v", err)
	}
	defer lg.close()
	if lg.lineage() != lineageUnknown || lg.size != 0 {
		t.Errorf("absent log reported lineage %d size %d", lg.lineage(), lg.size)
	}
}
