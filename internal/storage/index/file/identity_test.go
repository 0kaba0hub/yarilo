package file_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func dropFolderIndexes(t *testing.T, home string) {
	t.Helper()
	removed := 0
	if err := filepath.Walk(home, func(p string, fi os.FileInfo, e error) error {
		if e != nil || fi.IsDir() {
			return nil //nolint:nilerr
		}
		if strings.HasPrefix(filepath.Base(p), "yarilo.index") {
			if rerr := os.Remove(p); rerr != nil {
				t.Fatal(rerr)
			}
			removed++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatal("no index files were removed, so the test proves nothing")
	}
}

// Losing a folder's index used to lose its identity: the folder came back with
// a new UIDVALIDITY and every client resynchronised from scratch -- throwing
// away exactly what adoption spends effort keeping (#1611).
func TestAFolderKeepsItsUIDValidityAcrossIndexLoss(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}

	idx := indexfile.New().OpenUser(info)
	before, err := idx.OpenFolder("Work", 1788252508)
	if err != nil {
		t.Fatal(err)
	}
	idx.Close() //nolint:errcheck

	dropFolderIndexes(t, home)

	idx2 := indexfile.New().OpenUser(info)
	defer idx2.Close() //nolint:errcheck
	after, err := idx2.OpenFolder("Work", 1788299999)
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDValidity != before.UIDValidity {
		t.Errorf("uidvalidity was %d and came back %d; the record knows this folder is not new",
			before.UIDValidity, after.UIDValidity)
	}
}

// The other half, and the one a wrong fix would break: a folder deleted and
// created again is NOT the same folder, whatever the record remembers about the
// name. RFC 3501 §6.3.4.
func TestAFolderDeletedAndCreatedAgainDoesNotInheritItsIdentity(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck

	first, err := idx.OpenFolder("Work", 1788252508)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.DeleteFolder("Work"); err != nil {
		t.Fatal(err)
	}
	second, err := idx.OpenFolder("Work", 1788252508)
	if err != nil {
		t.Fatal(err)
	}
	if second.UIDValidity == first.UIDValidity {
		t.Errorf("the folder came back with %d, the number its predecessor had", first.UIDValidity)
	}
}

// A rename keeps the identity, which is what a rename means to a client -- and
// it must keep it across a later index loss too, or the record and the tree
// disagree about a folder that never changed.
func TestARenamedFolderKeepsItsIdentityAcrossIndexLoss(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}

	idx := indexfile.New().OpenUser(info)
	before, err := idx.OpenFolder("Work", 1788252508)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.RenameFolder("Work", "Job"); err != nil {
		t.Fatal(err)
	}
	idx.Close() //nolint:errcheck

	dropFolderIndexes(t, home)

	idx2 := indexfile.New().OpenUser(info)
	defer idx2.Close() //nolint:errcheck
	after, err := idx2.OpenFolder("Job", 1788299999)
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDValidity != before.UIDValidity {
		t.Errorf("uidvalidity was %d before the rename and %d after the index loss",
			before.UIDValidity, after.UIDValidity)
	}
}

// A folder the record has never heard of is a folder as far as anything can
// tell: the tree wins, nobody is called, and it simply gets an identity from
// here on.
func TestAFolderTheRecordDoesNotKnowIsNotRefused(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	dir := filepath.Join(home, "sdbox", "mailboxes", "Old", "dbox-Mails")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("Old", 1788252508)
	if err != nil {
		t.Fatalf("a folder with no entry was refused: %v", err)
	}
	if f.UIDValidity == 0 {
		t.Error("it opened without an identity")
	}
}
