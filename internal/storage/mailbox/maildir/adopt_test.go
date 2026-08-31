package maildir_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A store another implementation left keeps its UID space when this server
// takes it over.
//
// The uidlist is the same file in both -- version 3, "3 V<uidvalidity>
// N<nextuid>" and then "<uid> :<filename>" -- which is why it is adopted under
// our name rather than converted. What was not adopted was what it says: the
// numbers were parsed past, so the folder got a fresh UIDVALIDITY and fresh
// UIDs, and every client refetched every mailbox (#1593).
func TestAdoptingAMaildirKeepsItsUIDs(t *testing.T) {
	home := t.TempDir()
	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{
		"1700000001.M1P1.host,S=20:2,S",
		"1700000002.M2P2.host,S=20:2,",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Their uidlist, written the way they write it: the name is cut at the info
	// separator, so the record names the base and not the file. That is the
	// point of this fixture -- with full names in it the test would agree with
	// itself and pass over a store no other implementation produces
	// (maildir-uidlist.c cuts at MAILDIR_INFO_SEP before recording).
	uidlist := fmt.Sprintf("3 V1600000000 N42 G0123456789abcdef0123456789abcdef\n40 :%s\n41 :%s\n",
		maildirBaseOf(files[0]), maildirBaseOf(files[1]))
	if err := os.WriteFile(filepath.Join(home, "Maildir", "dovecot-uidlist"), []byte(uidlist), 0o600); err != nil {
		t.Fatal(err)
	}

	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	syncer, ok := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if !ok {
		t.Fatal("the maildir driver no longer reconciles")
	}
	if _, err := syncer.ReconcileIndex(idx, f); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	after, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDValidity != 1600000000 {
		t.Errorf("UIDVALIDITY is %d, and their uidlist says 1600000000", after.UIDValidity)
	}
	if after.NextUID != 42 {
		t.Errorf("next uid is %d, and their uidlist says 42", after.NextUID)
	}

	msgs, err := idx.GetMessages(after.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	for i, want := range []uint32{40, 41} {
		if msgs[i].UID != want {
			t.Errorf("message %d has uid %d, want %d -- their number, not a fresh one", i+1, msgs[i].UID, want)
		}
	}
	// Flags still come from the filenames, which is what made maildir work at
	// all before any of this.
	if !hasFlag(msgs[0].Flags, `\Seen`) {
		t.Errorf("uid 40 has flags %v, and its filename says \\Seen", msgs[0].Flags)
	}
}

// A file the uidlist does not know -- delivered by an MDA after the takeover --
// gets the next UID, as it always did.
func TestAFileTheUIDListDoesNotKnowGetsTheNextUID(t *testing.T) {
	home := t.TempDir()
	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	known := "1700000001.M1P1.host,S=20:2,S"
	fresh := "1700000009.M9P9.host,S=20:2,"
	for _, name := range []string{known, fresh} {
		if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	uidlist := fmt.Sprintf("3 V1600000000 N42 G0123456789abcdef0123456789abcdef\n40 :%s\n", maildirBaseOf(known))
	if err := os.WriteFile(filepath.Join(home, "Maildir", "dovecot-uidlist"), []byte(uidlist), 0o600); err != nil {
		t.Fatal(err)
	}

	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, _ := idx.OpenFolder("INBOX", 0)
	syncer := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if _, err := syncer.ReconcileIndex(idx, f); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after, _ := idx.OpenFolder("INBOX", 0)
	msgs, err := idx.GetMessages(after.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	byName := map[string]uint32{}
	for _, m := range msgs {
		byName[m.Filename] = m.UID
	}
	if byName[known] != 40 {
		t.Errorf("the file their list names has uid %d, want 40", byName[known])
	}
	if byName[fresh] < 42 {
		t.Errorf("the file their list does not name has uid %d; it should come after their next uid, 42", byName[fresh])
	}
}

func hasFlag(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// maildirBaseOf is what the other implementation records: the name up to the
// info separator.
func maildirBaseOf(name string) string {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i]
	}
	return name
}
