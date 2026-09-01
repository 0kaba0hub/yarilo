package maildir

import (
	"os"

	"path/filepath"
	"strings"
	"sync"
	"testing"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"

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

// recSetupLocked is recSetup with a locker wired, so acquisitions can be
// counted.
func recSetupLocked(t *testing.T) (*userMailbox, mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	root := t.TempDir()
	const user = "u@x.com"
	home := testHome(root, user)
	info := &mailbox.UserInfo{Username: user, Home: home}
	box := New(WithLocker(&countingLocker{})).OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatalf("create: %v", err)
	}
	idx := fileidx.New().OpenUser(info)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	return box, idx, folder
}

func hasFlagIn(all []string, want string) bool {
	for _, f := range all {
		if f == want {
			return true
		}
	}
	return false
}

// The legacy uidlist migration is a rename, and it is reached from paths that
// hold no lock -- the reconcile's scan among them since #1626. Two of them at
// once means one renames and the other finds the source gone: that is the
// migration having happened, not a folder that cannot be read.
func TestTheLegacyUIDListMigrationToleratesLosingTheRace(t *testing.T) {
	box, _ := batchBox(t)
	legacy := filepath.Join(box.folderPath("INBOX"), LegacyUIDListFileName)
	if err := os.WriteFile(legacy, []byte("3 V1 N1 G0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The loser's picture exactly: the source is gone and the destination is
	// there, because somebody else just did the rename.
	if err := os.Rename(legacy, box.uidListPath("INBOX")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("3 V1 N1 G0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(box.uidListPath("INBOX")); err != nil {
		t.Fatal(err)
	}

	// Now race it for real: several goroutines migrating the same folder.
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = box.migrateLegacyUIDList("INBOX")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("migration %d failed while another one succeeded: %v", i, err)
		}
	}
	if _, err := os.Stat(box.uidListPath("INBOX")); err != nil {
		t.Errorf("the uidlist is not there after the race: %v", err)
	}
}

// The folder cache is reached from the scan, which holds no mailbox lock since
// #1626. Two goroutines on one handle must not race it.
//
// Scanning alone is not enough to show it: with the directory unchanged every
// scan takes the cached slice and only reads. The writes happen when the
// directory moves under them, so the test keeps delivering while it scans --
// otherwise it asserts a property nobody exercised.
func TestTheFolderCacheIsSafeWithoutTheMailboxLock(t *testing.T) {
	box, _ := batchBox(t)
	deliverToCur(t, box, "1700000001.M1Pa.host:2,", "From: a@b\r\n\r\nx\r\n")

	done := make(chan struct{})
	var writer, scanners sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			// A real save, so the uidlist moves as well as the directory --
			// that file is what the cache holds, and a writer that only drops
			// message files never makes the cache write anything.
			body := "From: a@b\r\n\r\nx\r\n"
			_, _, _, _ = box.Save("INBOX", strings.NewReader(body), uint32(i+2), int64(len(body)), nil, [16]byte{})
		}
	}()

	for i := 0; i < 8; i++ {
		scanners.Add(1)
		go func() {
			defer scanners.Done()
			for j := 0; j < 50; j++ {
				if _, err := box.Scan("INBOX"); err != nil {
					t.Errorf("scan: %v", err)
					return
				}
			}
		}()
	}
	scanners.Wait()
	close(done)
	writer.Wait()
}

// The map the cache hands out must not be written into afterwards.
//
// snapshotUIDs returns the cached map itself, and a scan holds it while it
// works -- holding no folder lock since #1626. A delivery writing into that
// same map under the lock is still a concurrent write to a map somebody is
// reading, which the detector reports and a plain run cannot see at all.
//
// The uidlist has to exist first: with no file, readUIDList builds a map it
// never caches, nothing is shared, and the test passes without proving
// anything -- which is how the first version of it read.
func TestTheCachedUIDMapIsNotWrittenIntoAfterItEscapes(t *testing.T) {
	box, _ := batchBox(t)
	body := "From: a@b\r\n\r\nx\r\n"
	if _, _, _, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	m, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatal("the uidlist is empty, so the map is not the cached one and this proves nothing")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			_, _, _, _ = box.Save("INBOX", strings.NewReader(body), uint32(i+2), int64(len(body)), nil, [16]byte{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			for k := range m {
				_ = m[k]
			}
		}
	}()
	wg.Wait()
}

// A reconcile of a folder with an empty new/ takes the folder lock once, not
// twice.
//
// #1628 split the section in two and took the lock for both halves, including
// the half that had nothing to do -- and every acquisition is a round trip to
// the lock service, paid on every poll of every session (#1630).
//
// The probe is what makes this a real assertion: without it the test would pass
// on a folder that skipped the move because there was nothing to reconcile at
// all, which proves nothing about the acquisition it was written for.
func TestAReconcileWithNothingInNewTakesTheLockOnce(t *testing.T) {
	box, idx, folder := recSetupLocked(t)

	// A message already in cur/ and unknown to the index, so the pass has work
	// to do -- and new/ is empty, so the move phase must not take the lock.
	deliverToCur(t, box, "1700000001.M1Pa.host:2,", "From: a@b\r\n\r\nx\r\n")

	var moveTaken bool
	movePhaseProbe = func(taken bool) { moveTaken = taken }
	defer func() { movePhaseProbe = nil }()

	l := box.b.locker.(*countingLocker)
	before := l.acquires.Load()
	st, err := box.ReconcileIndex(idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	got := l.acquires.Load() - before

	if st.Imported != 1 {
		t.Fatalf("the pass imported %d messages, want 1 -- it must have work to do", st.Imported)
	}
	if moveTaken {
		t.Fatal("the move phase ran although new/ is empty, so this measures the wrong thing")
	}
	if got != 1 {
		t.Errorf("the reconcile took the folder lock %d times, want 1", got)
	}
}

// And when new/ does hold something, the move happens: the saving must not cost
// a delivery its migration.
func TestAReconcileWithNewMailStillMovesIt(t *testing.T) {
	box, idx, folder := recSetupLocked(t)
	deliverToNew(t, box, "1700000002.M1Pb.host", "From: a@b\r\n\r\nx\r\n")

	var moveTaken bool
	movePhaseProbe = func(taken bool) { moveTaken = taken }
	defer func() { movePhaseProbe = nil }()

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	if !moveTaken {
		t.Error("the move phase was skipped although new/ held a message")
	}
	entries, err := os.ReadDir(filepath.Join(box.folderPath("INBOX"), "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("new/ still holds %d files after the reconcile", len(entries))
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("index = %v, err = %v", msgs, err)
	}
	// And the name it recorded is one Fetch can open, which is cur/ and only
	// cur/.
	if _, err := box.Fetch("INBOX", msgs[0].Filename, false); err != nil {
		t.Errorf("the index names %q, which cannot be read: %v", msgs[0].Filename, err)
	}
}

// A message that arrives in new/ after the check is not imported under a name
// Fetch cannot open. It waits for the next pass, which moves it first.
func TestAMessageArrivingInNewAfterTheCheckIsNotImportedFromThere(t *testing.T) {
	box, idx, folder := recSetupLocked(t)

	afterScan = func() { afterScan = nil }
	// The delivery lands after hasNewMail said the directory was empty, so the
	// scan sees it in new/.
	movePhaseProbe = func(taken bool) {
		movePhaseProbe = nil
		if !taken {
			deliverToNew(t, box, "1700000003.M1Pc.host", "From: a@b\r\n\r\nx\r\n")
		}
	}
	defer func() { movePhaseProbe = nil; afterScan = nil }()

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if _, ferr := box.Fetch("INBOX", m.Filename, false); ferr != nil {
			t.Errorf("the index names %q, which Fetch cannot open: %v", m.Filename, ferr)
		}
	}

	// The next pass moves it and imports it properly.
	folder, err = idx.OpenFolder("INBOX", folder.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, err = idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("index = %v, err = %v", msgs, err)
	}
	if _, ferr := box.Fetch("INBOX", msgs[0].Filename, false); ferr != nil {
		t.Errorf("after the next pass %q still cannot be read: %v", msgs[0].Filename, ferr)
	}
}

// A new/ that exists and cannot be read must fail the reconcile, not be treated
// as empty.
//
// Skipping the phase on any error is silent in the worst way: only cur/ is
// imported from, so the mail in new/ never reaches the index and nothing is
// reported. Before the skip existed such a store failed on every pass, loudly,
// which is the behaviour to keep (#1630).
func TestAnUnreadableNewDirectoryFailsRatherThanBeingSkipped(t *testing.T) {
	box, idx, folder := recSetupLocked(t)
	newDir := filepath.Join(box.folderPath("INBOX"), "new")
	if err := os.RemoveAll(newDir); err != nil {
		t.Fatal(err)
	}
	// A plain file where the directory should be: ReadDir fails with something
	// that is not "does not exist", on every platform and without depending on
	// who the test runs as.
	if err := os.WriteFile(newDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	var moveTaken bool
	movePhaseProbe = func(taken bool) { moveTaken = taken }
	defer func() { movePhaseProbe = nil }()

	if _, err := box.ReconcileIndex(idx, folder); err == nil {
		t.Error("the reconcile succeeded although new/ cannot be read")
	}
	if !moveTaken {
		t.Error("the move phase was skipped, so the fault was never reported")
	}
}
