package file_test

import (
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A folder deleted and created again inside one second must not come back with
// the same UIDVALIDITY: a client that cached UIDs for the old one would keep
// treating them as valid for different mail (RFC 3501 §6.3.4, #1614).
//
// The stamp is passed in rather than read from the clock, which is exactly the
// case a real delete-and-create inside one second produces.
func TestAFolderRecreatedWithTheSameStampGetsANewUIDValidity(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck

	const stamp = uint32(1788252508)
	first, err := idx.OpenFolder("Work", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.DeleteFolder("Work"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	second, err := idx.OpenFolder("Work", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if second.UIDValidity == first.UIDValidity {
		t.Errorf("both incarnations carry uidvalidity %d; the second must differ", first.UIDValidity)
	}
	if second.UIDValidity < first.UIDValidity {
		t.Errorf("the second incarnation went backwards: %d then %d", first.UIDValidity, second.UIDValidity)
	}
}

// Two folders created in the same second are the same case without a delete.
func TestTwoFoldersCreatedInOneSecondDiffer(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck

	const stamp = uint32(1788252508)
	a, err := idx.OpenFolder("A", stamp)
	if err != nil {
		t.Fatal(err)
	}
	b, err := idx.OpenFolder("B", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if a.UIDValidity == b.UIDValidity {
		t.Errorf("two folders share uidvalidity %d", a.UIDValidity)
	}
}
