package file

import (
	"os"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestConcurrentCompactionNoUIDRegression reproduces #644: two pods share the
// same on-disk index. Pod A holds an open logFD; pod B advances the folder and
// compacts, which replaces the .log via truncateLog's rename (new inode) and
// rewrites the base with a higher NextUID. Pod A's cached logFD is now stale
// (points at the old, unlinked inode). Without the inode-identity check in
// reload(), pod A would fast-path past the change, keep its low in-memory
// NextUID, and eventually flush it back — regressing the counter. This test
// pins that reload() drops the stale fd, reconciles the advanced NextUID, and
// that pod A's next append lands in the live log (visible to a fresh reader).
func TestConcurrentCompactionNoUIDRegression(t *testing.T) {
	dir := t.TempDir()

	// Pod A: create the folder and one message. Leaves a logFD open on inode I1.
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
	if fsA.logFD == nil {
		t.Fatal("expected podA to hold an open logFD after append")
	}

	// Pod B (separate backend, same dir): advance the folder to UID 5, then
	// compact — flush() rewrites the base with NextUID=6 and truncateLog
	// replaces the .log with a fresh inode I2.
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

	// Simulate the stat coincidence the bug depends on: pretend pod A already
	// observed the base's current mtime (coarse mtime resolution / NFS attr
	// cache), so ONLY the log-inode identity can reveal the change. Without the
	// fix, reload() would fast-path here and keep NextUID=2.
	if st, _ := os.Stat(fsA.indexPath); st != nil {
		fsA.mu.Lock()
		fsA.baseMod = st.ModTime()
		fsA.mu.Unlock()
	}

	// Pod A reloads. The stale logFD (I1) must be recognised as replaced,
	// dropped, and the advanced NextUID picked up from the rewritten base.
	fsA.mu.Lock()
	if err := fsA.reload(true); err != nil {
		fsA.mu.Unlock()
		t.Fatalf("podA reload: %v", err)
	}
	got := fsA.file.Header.NextUID
	stillStaleFD := fsA.logFD != nil
	fsA.mu.Unlock()

	if got < 6 {
		t.Fatalf("NextUID regressed/stale after concurrent compaction: got %d, want >= 6", got)
	}
	if stillStaleFD {
		t.Fatal("podA kept its stale logFD after the log was replaced")
	}

	// Pod A's next append must land in the live log — a fresh reader must see
	// all six UIDs (1..5 plus the new one), proving the write did not go into
	// the dead inode.
	uid, err := podA.AllocateUID(fa.ID)
	if err != nil {
		t.Fatalf("podA AllocateUID: %v", err)
	}
	if uid < 6 {
		t.Fatalf("allocated UID regressed: got %d, want >= 6", uid)
	}
	ms, _ = podA.NextModSeq(fa.ID)
	if err := podA.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: uid, ModSeq: ms, Filename: "new.eml", Size: 100}); err != nil {
		t.Fatalf("podA append new UID: %v", err)
	}

	reader := openIdx(dir, testUser)
	fr, err := reader.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatalf("reader OpenFolder: %v", err)
	}
	msgs, err := reader.GetMessages(fr.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("reader GetMessages: %v", err)
	}
	seen := map[uint32]bool{}
	for _, m := range msgs {
		seen[m.UID] = true
	}
	for want := uint32(1); want <= 5; want++ {
		if !seen[want] {
			t.Errorf("fresh reader missing UID %d after reconcile", want)
		}
	}
	if !seen[uid] {
		t.Errorf("fresh reader missing podA's post-reconcile UID %d (append went to dead inode)", uid)
	}
}
