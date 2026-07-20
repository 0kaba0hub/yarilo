package file

import (
	"os"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestReloadDetectsBaseReplacedUnderSameMtime reproduces #666: reload()'s
// staleness check for the base .index compared mtime only. Two different
// files (the pre- and post-OptimizeIndex base, rewritten via Recreate's
// tmp+rename) can share an mtime tick on filesystems with coarse mtime
// resolution — the exact same anti-pattern already fixed for the .log file
// via inode identity (#645). This pins that reload() now also detects a
// same-mtime base replacement via os.SameFile and does a full reload instead
// of trusting a stale in-memory NextUID.
func TestReloadDetectsBaseReplacedUnderSameMtime(t *testing.T) {
	dir := t.TempDir()

	podA := openIdx(dir, testUser)
	fa, err := podA.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatalf("podA OpenFolder: %v", err)
	}
	ms, _ := podA.NextModSeq(fa.ID)
	if err := podA.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: 1, ModSeq: ms, Filename: "1.eml", Size: 100}); err != nil {
		t.Fatalf("podA append UID1: %v", err)
	}
	fsA := podA.open[fa.ID]

	podB := openIdx(dir, testUser)
	fb, err := podB.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatalf("podB OpenFolder: %v", err)
	}
	for uid := uint32(2); uid <= 5; uid++ {
		ms, _ := podB.NextModSeq(fb.ID)
		if err := podB.AppendMessage(fb.ID, &mailbox.MessageMeta{UID: uid, ModSeq: ms, Filename: "x.eml", Size: 100}); err != nil {
			t.Fatalf("podB append UID%d: %v", uid, err)
		}
	}
	if err := podB.OptimizeIndex(fb.ID); err != nil {
		t.Fatalf("podB compact: %v", err)
	}

	// Simulate the stat coincidence the bug depends on: pod A's cached base
	// identity is the PRE-optimize file, but its cached mtime and log-size are
	// forced to match the POST-optimize disk state (coarse mtime resolution /
	// NFS attr cache) — and its logFD is dropped so the .log identity check
	// cannot itself catch the change. Only the base inode-identity check can
	// now reveal that the base file was replaced.
	baseStat, err := os.Stat(fsA.indexPath)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}
	logStat, err := os.Stat(fsA.indexPath + ".log")
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	fsA.mu.Lock()
	fsA.closeFDs()
	fsA.baseMod = baseStat.ModTime()
	fsA.logSize = logStat.Size()
	fsA.mu.Unlock()

	fsA.mu.Lock()
	if err := fsA.reload(); err != nil {
		fsA.mu.Unlock()
		t.Fatalf("podA reload: %v", err)
	}
	got := fsA.file.Header.NextUID
	fsA.mu.Unlock()

	if got < 6 {
		t.Fatalf("NextUID stale after same-mtime base replacement: got %d, want >= 6 (fast path wrongly trusted stale base)", got)
	}
}
