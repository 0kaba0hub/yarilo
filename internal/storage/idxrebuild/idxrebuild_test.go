package idxrebuild_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func home(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

// TestRebuildFolder covers the three record fates: a file the index knows keeps
// its UID; a file the index has never seen gets a fresh UID; a record whose file
// vanished is dropped.
func TestRebuildFolder(t *testing.T) {
	root := t.TempDir()
	const user = "u@x.com"
	info := &mailbox.UserInfo{Username: user, Home: home(root, user)}

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	keep, _, _, err := box.Save("INBOX", strings.NewReader("a\n"), 1, 2, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	gone, _, _, err := box.Save("INBOX", strings.NewReader("b\n"), 2, 2, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: 1, Filename: keep, Size: 2, VSize: 2}); err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: 2, Filename: gone, Size: 2, VSize: 2}); err != nil {
		t.Fatal(err)
	}
	// UID 2's file vanishes; a brand-new file appears that the index never saw.
	if err := box.Remove("INBOX", gone); err != nil {
		t.Fatal(err)
	}
	fresh, _, _, err := box.Save("INBOX", strings.NewReader("c\n"), 0, 2, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}

	folder, _ = idx.OpenFolder("INBOX", 1)
	st, err := idxrebuild.RebuildFolder(box, idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.UIDsPreserved != 1 || st.UIDsAssigned != 1 {
		t.Fatalf("stats = %+v, want preserved=1 assigned=1", st)
	}

	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	byName := map[string]*mailbox.MessageMeta{}
	for _, m := range msgs {
		byName[m.Filename] = m
	}
	if k := byName[keep]; k == nil || k.UID != 1 {
		t.Fatalf("kept message wrong: %+v", byName[keep])
	}
	if _, ok := byName[gone]; ok {
		t.Fatal("vanished message still indexed")
	}
	if f := byName[fresh]; f == nil || f.UID != 3 {
		t.Fatalf("fresh message UID = %v, want 3", byName[fresh])
	}
}

// TestExpungeMissing: the reactive heal drops only records whose file vanished,
// keeps present ones with their UID, and does NOT import an orphan file on disk
// that the index never knew (that is operator-rebuild territory).
func TestExpungeMissing(t *testing.T) {
	root := t.TempDir()
	const user = "u@x.com"
	info := &mailbox.UserInfo{Username: user, Home: home(root, user)}

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	keep, _, _, err := box.Save("INBOX", strings.NewReader("a\n"), 1, 2, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	gone, _, _, err := box.Save("INBOX", strings.NewReader("b\n"), 2, 2, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	for uid, n := range map[uint32]string{1: keep, 2: gone} {
		if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uid, Filename: n, Size: 2, VSize: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := box.Remove("INBOX", gone); err != nil {
		t.Fatal(err)
	}
	// An orphan appears on disk that the index never saw — must be left alone.
	if _, _, _, err := box.Save("INBOX", strings.NewReader("c\n"), 0, 2, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}

	folder, _ = idx.OpenFolder("INBOX", 1)
	n, err := idxrebuild.ExpungeMissing(box, idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(n) != 1 {
		t.Fatalf("expunged = %d, want 1", len(n))
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 1 || msgs[0].UID != 1 || msgs[0].Filename != keep {
		t.Fatalf("after heal: %+v, want only UID 1 (%s)", msgs, keep)
	}
}

// A crash between the index expunge and the unlink leaves a file with no record,
// which the next rebuild re-files as a new message (#1690).
func TestARebuildRefilesAFileWhoseRecordWasExpunged(t *testing.T) {
	root := t.TempDir()
	const user = "crash@x.com"
	info := &mailbox.UserInfo{Username: user, Home: home(root, user)}

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 1, 5, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.AllocateAndAppend(folder.ID, &mailbox.MessageMeta{Filename: name, Size: 5}); err != nil {
		t.Fatal(err)
	}
	before, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(before) != 1 {
		t.Fatalf("setup: %d records, %v", len(before), err)
	}

	// The crash: the record is gone, the file is not.
	if err := idx.ExpungeMessage(folder.ID, before[0].UID); err != nil {
		t.Fatal(err)
	}

	folder, err = idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := idxrebuild.RebuildFolder(box, idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.UIDsAssigned != 1 {
		t.Errorf("the rebuild assigned %d fresh UIDs, want 1: the orphan file is not "+
			"re-filed, so the documented consequence is not what happens", st.UIDsAssigned)
	}
	after, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("after the rebuild the folder holds %d records, want 1", len(after))
	}
	if after[0].UID == before[0].UID {
		t.Errorf("the message came back with its old UID %d; a re-filed orphan gets a "+
			"fresh one", after[0].UID)
	}
}
