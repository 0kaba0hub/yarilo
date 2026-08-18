package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The expunge record's GUID field is the EXPUNGED MESSAGE's identity -- the
// only place it survives, since the record it came from is being removed. It
// used to be written with the mailbox's GUID, which gave every expunge in a
// folder the same value: QRESYNC never noticed, because it matches by UID, and
// no other reader existed until JMAP needed to say what was destroyed (#1216).
func TestExpungeRecordCarriesTheMessageGUID(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	guids := map[uint32][16]byte{
		1: {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		2: {16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
	}
	for uid := uint32(1); uid <= 2; uid++ {
		if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: "m.eml", Size: 10, GUID: guids[uid],
		}); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	for uid := uint32(1); uid <= 2; uid++ {
		if err := b.ExpungeMessage(f.ID, uid); err != nil {
			t.Fatalf("ExpungeMessage: %v", err)
		}
	}

	got, complete, err := b.VanishedGUIDs(f.ID, 0)
	if err != nil {
		t.Fatalf("VanishedGUIDs: %v", err)
	}
	if !complete {
		t.Fatal("the records this build wrote could not be named")
	}
	if len(got) != 2 {
		t.Fatalf("got %d vanished ids, want 2: %v", len(got), got)
	}
	// Two different messages must produce two different ids. The old behaviour
	// produced the same value twice, which is what a mailbox-wide GUID gives.
	if got[0] == got[1] {
		t.Fatalf("both expunges report the same id %x -- the field is not the message's", got[0])
	}
	seen := map[[16]byte]bool{got[0]: true, got[1]: true}
	for uid, want := range guids {
		if !seen[want] {
			t.Errorf("uid %d expunged as something other than its own GUID %x", uid, want)
		}
	}
}

// A record written before the fix holds the mailbox GUID, which cannot be told
// from a real id except by that equality. Those are dropped and the answer is
// reported as partial, so a caller degrades instead of publishing an id that
// names a mailbox and no message.
func TestVanishedGUIDsReportsLegacyRecordsAsIncomplete(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	fs := b.open[f.ID]
	mailboxGUID := fs.hdr.MailboxGUID
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: "m.eml", Size: 10, GUID: mailboxGUID,
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := b.ExpungeMessage(f.ID, 1); err != nil {
		t.Fatalf("ExpungeMessage: %v", err)
	}

	got, complete, err := b.VanishedGUIDs(f.ID, 0)
	if err != nil {
		t.Fatalf("VanishedGUIDs: %v", err)
	}
	if complete {
		t.Error("a record carrying the mailbox GUID was reported as a usable message id")
	}
	if len(got) != 0 {
		t.Errorf("got %v, want the unnameable record dropped", got)
	}
}
