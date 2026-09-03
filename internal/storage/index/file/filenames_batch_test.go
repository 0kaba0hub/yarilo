package file

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A whole command's renames cost one acquisition, not one each.
//
// On maildir a flag change renames the file, so a STORE over 40 messages
// recorded 40 new names and took the index's exclusive lock 40 times. Measured
// at 121ms per name on a contended folder, which is how a single STORE reached
// 18 seconds while every other part of it stayed in the tens of milliseconds
// (#1646).
func TestRecordingAWholeBatchOfNamesTakesOneLock(t *testing.T) {
	dir := t.TempDir()
	newLocker := raceTestLockServer(t)
	const user = "batch@example.com"
	ui := New(WithLocker(newLocker())).OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user),
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	names := make(map[uint32]string, n)
	for uid := uint32(1); uid <= n; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: fmt.Sprintf("%d:2,", uid), Size: 10, VSize: 10,
		}); err != nil {
			t.Fatal(err)
		}
		names[uid] = fmt.Sprintf("%d:2,S", uid)
	}

	before := counterVal(t, metricLockAcquired, "exclusive", lockSiteWrite)
	if err := ui.UpdateFilenames(f.ID, names); err != nil {
		t.Fatalf("UpdateFilenames: %v", err)
	}
	if got := counterVal(t, metricLockAcquired, "exclusive", lockSiteWrite) - before; got != 1 {
		t.Errorf("recording %d names took %v exclusive acquisitions, want 1", n, got)
	}

	// The zeros above are worth nothing unless every name actually landed: a
	// call that recorded nothing also takes one lock.
	msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n {
		t.Fatalf("index holds %d messages, want %d", len(msgs), n)
	}
	for _, m := range msgs {
		if want := names[m.UID]; m.Filename != want {
			t.Errorf("uid %d is named %q, want %q -- the batch reported success without writing",
				m.UID, m.Filename, want)
		}
	}
}

// A name the folder does not carry is skipped, and the rest still land: the
// batch must not be all-or-nothing on a uid another session expunged between
// the rename and the record.
func TestABatchSkipsAnUnknownUIDAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	const user = "batch2@example.com"
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: user, Home: testHome(dir, user),
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 3; uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: fmt.Sprintf("%d:2,", uid), Size: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	err = ui.UpdateFilenames(f.ID, map[uint32]string{
		1: "1:2,S", 99: "99:2,S", 3: "3:2,S",
	})
	if err != nil {
		t.Fatalf("a batch with one unknown uid failed outright: %v", err)
	}
	msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint32]string{}
	for _, m := range msgs {
		got[m.UID] = m.Filename
	}
	for uid, want := range map[uint32]string{1: "1:2,S", 2: "2:2,", 3: "3:2,S"} {
		if got[uid] != want {
			t.Errorf("uid %d is named %q, want %q", uid, got[uid], want)
		}
	}
}
