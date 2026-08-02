package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RFC 8474: EMAILID is unique per message and unchanged by MOVE.

func newGUIDBackend(t *testing.T, driver, home string) mailbox.UserMailbox {
	t.Helper()
	user := &mailbox.UserInfo{Username: "guid@example.com", Home: home}
	switch driver {
	case "maildir":
		return maildir.New().OpenUser(user)
	case "mdbox":
		return mdbox.New().OpenUser(user)
	case "sdbox":
		return dboxv2.New().OpenUser(user)
	}
	t.Fatalf("unknown driver %q", driver)
	return nil
}

// TestGUIDIsRealAndStable checks every mailbox format: a stored message gets a
// non-zero unique GUID and MOVE keeps it.
func TestGUIDIsRealAndStable(t *testing.T) {
	var zero [16]byte
	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			home := t.TempDir()
			mb := newGUIDBackend(t, driver, home)
			t.Cleanup(func() { _ = mb.Close() })
			if err := mb.Init(); err != nil {
				t.Fatalf("init: %v", err)
			}
			if err := mb.Create("Archive"); err != nil {
				t.Fatalf("create Archive: %v", err)
			}

			body := "Subject: t\r\n\r\nbody\r\n"
			name1, _, guid1, err := mb.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil, zero)
			if err != nil {
				t.Fatalf("save: %v", err)
			}
			if guid1 == zero {
				t.Fatal("Save returned a zero GUID: EMAILID would be all-zero")
			}

			name2, _, guid2, err := mb.Save("INBOX", strings.NewReader(body), 2, int64(len(body)), nil, zero)
			if err != nil {
				t.Fatalf("save 2: %v", err)
			}
			if guid2 == guid1 {
				t.Fatalf("two messages share GUID %x: EMAILID is not unique", guid1)
			}

			// Scan, not List: mdbox.List is empty by design. Storage must report
			// the GUID Save returned or a rebuild would change EMAILID.
			scanned, err := mb.Scan("INBOX")
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			byName := map[string][16]byte{}
			for _, r := range scanned {
				byName[r.Filename] = r.GUID
			}
			if got, ok := byName[name1]; !ok {
				t.Errorf("Scan did not report %q", name1)
			} else if got != guid1 {
				t.Errorf("Scan GUID = %x, Save returned %x", got, guid1)
			}

			// MOVE keeps the identity (RFC 8474).
			moved, movedGUID, err := mb.Move("INBOX", "Archive", name1, guid1)
			if err != nil {
				t.Fatalf("move: %v", err)
			}
			if movedGUID != guid1 {
				t.Errorf("MOVE changed EMAILID: %x -> %x", guid1, movedGUID)
			}
			archived, err := mb.Scan("Archive")
			if err != nil {
				t.Fatalf("scan Archive: %v", err)
			}
			found := false
			for _, r := range archived {
				if r.Filename == moved {
					found = true
					if r.GUID != guid1 {
						t.Errorf("after MOVE stored GUID = %x, want %x", r.GUID, guid1)
					}
				}
			}
			if !found {
				t.Errorf("moved message %q not listed in Archive", moved)
			}
			_ = name2
		})
	}
}

// TestGUIDSurvivesFlagChange: the maildir GUID derives from the base name, so
// rewriting the ":2," trailer must not change EMAILID.
func TestGUIDSurvivesFlagChange(t *testing.T) {
	home := t.TempDir()
	mb := newGUIDBackend(t, "maildir", home)
	t.Cleanup(func() { _ = mb.Close() })
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	body := "Subject: t\r\n\r\nbody\r\n"
	name, _, guid, err := mb.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// A flag change renames only the trailer, which is what an IMAP STORE does.
	cur := filepath.Join(home, "Maildir", "cur")
	flagged := name + "S"
	if err := os.Rename(filepath.Join(cur, name), filepath.Join(cur, flagged)); err != nil {
		t.Fatalf("rename for flag change: %v", err)
	}

	msgs, err := mb.List("INBOX")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range msgs {
		if m.Filename == flagged && m.GUID != guid {
			t.Fatalf("flag change altered EMAILID: %x -> %x", guid, m.GUID)
		}
	}
}

// TestGUIDReachesIndex walks the path FETCH uses: a GUID that never lands in an
// index record renders as all-zero.
func TestGUIDReachesIndex(t *testing.T) {
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "guid@example.com", Home: home}
	mb := maildir.New().OpenUser(user)
	idx := file.New().OpenUser(user)
	t.Cleanup(func() { _ = idx.Close() })
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	uid, err := idx.AllocateUID(folder.ID)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	body := "Subject: t\r\n\r\nbody\r\n"
	name, vsize, guid, err := mb.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
		UID: uid, Filename: name, Size: uint32(len(body)), VSize: vsize, GUID: guid,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if got[0].GUID != guid {
		t.Fatalf("index GUID = %x, want %x (EMAILID would be wrong)", got[0].GUID, guid)
	}
	if got[0].GUID == ([16]byte{}) {
		t.Fatal("index returned a zero GUID: this is the shipped EMAILID bug")
	}
}
