package maildir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The folder lock is not held across a directory walk, and the stats taken
// under it follow the number of records the pass changes -- not the number of
// messages in the folder (#1626).
//
// Counted rather than described: a change that moves the scan back inside the
// section increments the walk counter, and one that stats every message
// increments the other past what the pass applied.
func TestTheReconcileSectionDoesNotWalkTheDirectory(t *testing.T) {
	box, idx, folder := recSetup(t)

	// Twenty messages in the folder, of which one is new to the index: the
	// pass applies one record and must not pay for the other nineteen.
	for i := 0; i < 19; i++ {
		name := "17000000" + string(rune('0'+i%10)) + ".M1P" + string(rune('a'+i)) + ".host"
		deliverToNew(t, box, name, "body\r\n")
	}
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	folder, err := idx.OpenFolder("INBOX", folder.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	deliverToNew(t, box, "1700000099.M1Pz.host", "body\r\n")

	var dirReads, stats int
	sectionProbe = func(d, s int) { dirReads, stats = d, s }
	defer func() { sectionProbe = nil }()
	box.sectionDir.Store(0)
	box.sectionFS.Store(0)

	st, err := box.ReconcileIndex(idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	applied := st.Imported + st.Expunged + st.Updated
	if applied != 1 {
		t.Fatalf("the pass applied %d records, want 1 (%+v)", applied, st)
	}
	if dirReads != 0 {
		t.Errorf("the critical section walked the directory %d times", dirReads)
	}
	if stats > applied {
		t.Errorf("the section took %d stats for %d applied records; it must follow the changes, not the folder", stats, applied)
	}
}

// The rule for a message renamed between the unlocked scan and the apply: its
// flags are not written from a name that is no longer on disk. The rename is
// made to land in that window on purpose -- the reconcile scans for itself, so
// a test that renames beforehand exercises nothing (#1626).
func TestFlagsAreNotWrittenFromANameThatMovedOn(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.M1Pa.host", "body\r\n")
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("index = %v, err = %v", msgs, err)
	}
	cur := filepath.Join(box.folderPath("INBOX"), "cur")
	name := msgs[0].Filename

	// Another writer sets \Seen. The scan will see that name.
	seen := renameWithFlags(name, "S")
	if err := os.Rename(filepath.Join(cur, name), filepath.Join(cur, seen)); err != nil {
		t.Fatal(err)
	}
	// And between the scan and the apply, a third state arrives.
	flagged := renameWithFlags(name, "F")
	afterScan = func() {
		afterScan = nil
		if rerr := os.Rename(filepath.Join(cur, seen), filepath.Join(cur, flagged)); rerr != nil {
			t.Errorf("rename in the window: %v", rerr)
		}
	}
	defer func() { afterScan = nil }()

	folder, err = idx.OpenFolder("INBOX", folder.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	after, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(after) != 1 {
		t.Fatalf("index = %v, err = %v", after, err)
	}
	if after[0].Filename == seen {
		t.Errorf("the index took %q, a name that was gone before it was written", seen)
	}
	if hasFlagIn(after[0].Flags, `\Seen`) {
		t.Errorf("flags are %v: written from the name the scan saw, not from the disk", after[0].Flags)
	}
}

func hasFlagIn(all []string, want string) bool {
	for _, f := range all {
		if f == want {
			return true
		}
	}
	return false
}
