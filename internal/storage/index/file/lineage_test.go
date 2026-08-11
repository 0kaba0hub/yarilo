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
	// The invariant is that the two are PAIRED, not that they carry the same
	// number. A log that existed before the base was written keeps its own
	// lineage and the base names it as the one it absorbed; only a log created
	// after the base carries the base's. Asserting equality would demand the
	// second case and fail on the first, which is the ordinary one right after
	// a stamp.
	if _, paired := replayStart(fs.lineage, lg.lineage()); !paired {
		t.Errorf("base lineage %d/folded %d and log lineage %d are not paired",
			fs.lineage.Lineage, fs.lineage.FoldedLineage, lg.lineage())
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

// The property has to arrive on folders that already exist, and it has to
// arrive without anyone writing to them. Otherwise a read-only workload never
// flushes, the lineage never appears, and every "lock-free" read falls back to
// the locked path forever — which is exactly what the first sandbox
// measurement showed: adopt zero, acquisitions unchanged.
func TestAnExistingFolderGainsItsLineageOnOpen(t *testing.T) {
	root := t.TempDir()
	user := "gina@example.com"

	// A folder as it exists before the upgrade: written, then stripped of the
	// extension the old code never wrote.
	ui := openIdx(root, user)
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "1", Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	fs := ui.open[f.ID]
	indexPath := fs.indexPath
	stripLineage(t, fs)

	// A fresh handle opens it, as a session would after the upgrade.
	ui2 := openIdx(root, user)
	f2, err := ui2.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	fs2 := ui2.open[f2.ID]
	if fs2.lineage.Lineage == lineageUnknown {
		t.Fatal("the folder still has no lineage after being opened")
	}
	lg, err := openLogRead(indexPath)
	if err != nil {
		t.Fatalf("openLogRead: %v", err)
	}
	defer lg.close()
	if _, paired := replayStart(fs2.lineage, lg.lineage()); !paired {
		t.Errorf("after stamping, base %d/folded %d does not pair with log %d",
			fs2.lineage.Lineage, fs2.lineage.FoldedLineage, lg.lineage())
	}

	// And it is once: a third open must not rewrite the base again.
	stamps := counterValue(t, metricLineageStamped)
	ui3 := openIdx(root, user)
	if _, err := ui3.OpenFolder("INBOX", 42, ""); err != nil {
		t.Fatalf("third open: %v", err)
	}
	if got := counterValue(t, metricLineageStamped); got != stamps {
		t.Errorf("opening a stamped folder stamped it again (%v -> %v)", stamps, got)
	}
}

// stripLineage rewrites the base without the extension, which is what every
// index written before it looks like.
func stripLineage(t *testing.T, fs *folderState) {
	t.Helper()
	exts := fs.file.Extensions[:0]
	for _, e := range fs.file.Extensions {
		if e.Name != extNameLineage {
			exts = append(exts, e)
		}
	}
	fs.file.Extensions = exts
	fs.lineage = lineageHdr{}
	// Recompute the header region: dropping an extension shortens it, and the
	// writer refuses a header whose declared size does not match its content.
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		t.Fatalf("strip: layout: %v", err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatalf("strip: ext headers: %v", err)
	}
	fs.file.Extensions = layout.Extensions
	fs.file.Layout = layout
	fs.file.Header.RecordSize = layout.RecordSize
	fs.file.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	ri := fs.file.ToRecreateInput(fs.indexPath)
	if _, err := mailindex.Recreate(ri); err != nil {
		t.Fatalf("strip: recreate: %v", err)
	}
}
