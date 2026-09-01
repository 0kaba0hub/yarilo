package file_test

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A foreign sdbox store is taken over in place: their folder index becomes
// ours, their message files stay exactly where they are, and theirs is removed
// once ours is on the disk (#1592).
//
// The fixtures are a store the reference wrote, and the numbers below are what
// its own server reported for that store.
func TestAForeignSdboxStoreIsAdopted(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"dovecot.index.log": dboxref.SdboxInboxLog(t)}
	for uid := 1; uid <= 4; uid++ {
		files[fmt.Sprintf("u.%d", uid)] = dboxref.SdboxInboxMessage(t, uid)
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer idx.Close() //nolint:errcheck

	// The uidvalidity asked for plays no part: a folder that exists in their
	// store is not a new folder.
	f, err := idx.OpenFolder("INBOX", 999)
	if err != nil {
		t.Fatalf("a foreign sdbox folder did not open: %v", err)
	}
	if f.UIDValidity != 1788252508 {
		t.Errorf("uidvalidity = %d, their server reported 1788252508", f.UIDValidity)
	}
	if f.NextUID != 6 {
		t.Errorf("next_uid = %d, their server reported 6 (uid 5 was expunged there)", f.NextUID)
	}

	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("index holds %d messages, their server reported 4", len(msgs))
	}
	want := []struct {
		uid      uint32
		file     string
		flags    string
		keywords string
		guid     string
	}{
		{1, "u.1", `\Seen`, "", "c2d304036291966a4a0000000a4d75c4"},
		{2, "u.2", `\Answered`, "", "692555046291966a4d0000000a4d75c4"},
		{3, "u.3", "", "$Important", "e13e86056291966a510000000a4d75c4"},
		{4, "u.4", "", "$Important $Label", "c919ef066291966a540000000a4d75c4"},
	}
	for i, w := range want {
		m := msgs[i]
		if m.UID != w.uid || m.Filename != w.file {
			t.Errorf("record %d = uid %d file %q, want uid %d file %q", i, m.UID, m.Filename, w.uid, w.file)
		}
		if got := strings.Join(m.Flags, " "); got != w.flags {
			t.Errorf("uid %d: flags = %q, their server reported %q", w.uid, got, w.flags)
		}
		if got := strings.Join(m.Keywords, " "); got != w.keywords {
			t.Errorf("uid %d: keywords = %q, their server reported %q", w.uid, got, w.keywords)
		}
	}

	// Their index carries no guid, so the conversion leaves the folder pending
	// and the backfill -- the same call a select makes -- stamps them from the
	// message files. Their own server reported these guids for these messages,
	// and a client that has them must keep them (#1573).
	box := dboxv2.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer box.Close() //nolint:errcheck
	if err := idxrebuild.BackfillGUIDs(box, idx, f, "INBOX"); err != nil {
		t.Fatalf("guid backfill: %v", err)
	}
	msgs, err = idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range want {
		if got := hex.EncodeToString(msgs[i].GUID[:]); got != w.guid {
			t.Errorf("uid %d: guid = %s, their server reported %s -- EMAILID changed under a client", w.uid, got, w.guid)
		}
	}

	// Their index is gone and ours is there. The message files are untouched:
	// the same files both servers read, which is what makes this in place
	// rather than a copy.
	if _, serr := os.Stat(filepath.Join(dir, "dovecot.index.log")); !os.IsNotExist(serr) {
		t.Errorf("their index survived the conversion: %v", serr)
	}
	for uid := 1; uid <= 4; uid++ {
		name := fmt.Sprintf("u.%d", uid)
		got, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("%s: %v", name, rerr)
		}
		if !bytes.Equal(got, dboxref.SdboxInboxMessage(t, uid)) {
			t.Errorf("%s changed during the conversion", name)
		}
	}
}

// A second open finds a folder of ours and converts nothing again.
func TestAnAdoptedSdboxFolderIsNotConvertedTwice(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dovecot.index.log"), dboxref.SdboxInboxLog(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for uid := 1; uid <= 4; uid++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("u.%d", uid)),
			dboxref.SdboxInboxMessage(t, uid), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	open := func() *mailbox.Folder {
		idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
		defer idx.Close() //nolint:errcheck
		f, err := idx.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return f
	}
	first := open()
	second := open()
	if first.UIDValidity != second.UIDValidity || second.NextUID != first.NextUID {
		t.Errorf("second open changed the folder: %+v then %+v", first, second)
	}
}

// A folder of ours under the sdbox driver still opens: with nothing of theirs
// in the directory there is nothing to convert.
func TestAnOrdinarySdboxFolderStillOpens(t *testing.T) {
	home := t.TempDir()
	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer idx.Close() //nolint:errcheck
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("an ordinary sdbox folder was refused: %v", err)
	}
}
