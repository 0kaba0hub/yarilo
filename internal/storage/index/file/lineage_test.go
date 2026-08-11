package file

import (
	"os"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The pairing is what a lock-free reader will stand on: the log says which base
// it belongs to, the base says how far into the previous log it already
// reaches. Both halves have to survive a reopen, or the reader has nothing to
// compare.
func TestBaseAndLogAgreeOnLineage(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "alice@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	fs := ui.open[f.ID]
	if fs.lineage.Lineage == lineageUnknown {
		t.Fatal("the base carries no lineage after a flush")
	}
	lg, err := openLogRead(fs.indexPath)
	if err != nil {
		t.Fatalf("openLogRead: %v", err)
	}
	defer lg.close()
	if got := lg.lineage(); got != fs.lineage.Lineage {
		t.Errorf("the log announces lineage %d, the base %d", got, fs.lineage.Lineage)
	}
}

// A base that absorbed a log records how far into it. Replaying from the start
// instead would re-apply everything the base already contains — survivable only
// while every transaction type happens to be idempotent, which is a property
// nobody declared.
func TestFoldedOffsetSurvivesTheCrashBetweenBaseAndTruncation(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "bob@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	for uid := uint32(1); uid <= 3; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "f", Size: 10}); err != nil {
			t.Fatalf("AppendMessage %d: %v", uid, err)
		}
	}
	fs := ui.open[f.ID]

	// Keep the log as it stands, then compact. The crash is putting the log
	// back: the base is durable, the truncation never happened.
	logPath := fs.indexPath + ".log"
	logBytes, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read log: %v", rerr)
	}
	if err := ui.OptimizeIndex(f.ID); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	foldedInto := fs.lineage
	if err := os.WriteFile(logPath, logBytes, 0o600); err != nil {
		t.Fatalf("restore log: %v", err)
	}

	if foldedInto.FoldedLineage == lineageUnknown {
		t.Fatal("the base does not name the log it absorbed")
	}
	off, paired := replayStart(foldedInto, foldedInto.FoldedLineage)
	if !paired {
		t.Fatal("a base and the log it absorbed were not recognised as paired")
	}
	if off < int64(mailindex.LogHeaderSize) {
		t.Errorf("replay would start at %d, before the log header", off)
	}
	if off != int64(foldedInto.FoldedOffset) {
		t.Errorf("replay starts at %d, the base recorded %d as absorbed", off, foldedInto.FoldedOffset)
	}
}

func TestReplayStartRefusesToGuess(t *testing.T) {
	tests := []struct {
		name        string
		base        lineageHdr
		logLineage  uint32
		wantPaired  bool
		wantAtStart bool
	}{
		{"own log replays whole", lineageHdr{Lineage: 7, FoldedLineage: 6, FoldedOffset: 500}, 7, true, true},
		{"absorbed log resumes", lineageHdr{Lineage: 7, FoldedLineage: 6, FoldedOffset: 500}, 6, true, false},
		{"a stranger's log", lineageHdr{Lineage: 7, FoldedLineage: 6}, 3, false, false},
		{"base predates the extension", lineageHdr{}, 7, false, false},
		{"log predates the extension", lineageHdr{Lineage: 7}, lineageUnknown, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			off, paired := replayStart(tc.base, tc.logLineage)
			if paired != tc.wantPaired {
				t.Fatalf("paired = %v, want %v", paired, tc.wantPaired)
			}
			if paired && tc.wantAtStart && off != int64(mailindex.LogHeaderSize) {
				t.Errorf("offset %d, want the log header size", off)
			}
			if paired && !tc.wantAtStart && off == int64(mailindex.LogHeaderSize) {
				t.Errorf("offset %d restarts the absorbed log", off)
			}
		})
	}
}
