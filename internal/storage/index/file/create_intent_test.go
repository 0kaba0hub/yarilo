package file_test

import (
	"os"
	"path/filepath"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Creating a folder and opening one are different questions, and only the
// second has a past to look for. A create that searched for another
// implementation's index would adopt a store into a folder somebody just asked
// for by name (#1608).
func TestCreatingAFolderDoesNotAdoptAForeignOne(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dovecot.index.log"), dboxref.SdboxInboxLog(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for uid := 1; uid <= 4; uid++ {
		if err := os.WriteFile(filepath.Join(dir, "u."+string(rune('0'+uid))),
			dboxref.SdboxInboxMessage(t, uid), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer idx.Close() //nolint:errcheck
	fc, ok := idx.(mailbox.FolderCreator)
	if !ok {
		t.Fatal("the index does not say it can create a folder")
	}

	f, err := fc.CreateFolder("INBOX", 12345)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if f.Messages != 0 {
		t.Errorf("a folder being created came back holding %d messages", f.Messages)
	}
	if f.UIDValidity != 12345 {
		t.Errorf("uidvalidity = %d, want the one the create asked for", f.UIDValidity)
	}
	// Theirs is untouched: a create is not a takeover, and the next open is
	// still free to adopt it.
	if _, serr := os.Stat(filepath.Join(dir, "dovecot.index.log")); serr != nil {
		t.Errorf("their index was consumed by a create: %v", serr)
	}
}

// The other half: an open still adopts.
func TestOpeningTheSameFolderStillAdopts(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dovecot.index.log"), dboxref.SdboxInboxLog(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for uid := 1; uid <= 4; uid++ {
		if err := os.WriteFile(filepath.Join(dir, "u."+string(rune('0'+uid))),
			dboxref.SdboxInboxMessage(t, uid), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 999)
	if err != nil {
		t.Fatal(err)
	}
	if f.Messages != 4 || f.UIDValidity != 1788252508 {
		t.Errorf("an open did not adopt: %d messages, uidvalidity %d", f.Messages, f.UIDValidity)
	}
}
