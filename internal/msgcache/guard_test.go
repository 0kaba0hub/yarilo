package msgcache

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// foreignBS is a BodyStructure the codec does not know. The interface has an
// unexported method, so the only way to hold one from outside go-imap is to
// embed the interface -- which is also the only way a future go-imap node
// kind would arrive here: as a type this package's switch does not match.
type foreignBS struct{ imaplib.BodyStructure }

// The write-side guard, pinned in both directions: a structure that does not
// survive its own codec is NOT stored, and an ordinary one is. Without the
// guard the unknown kind encodes to a sentinel byte, gets stamped, and turns
// the message into a permanent decode miss for every later FETCH -- worse
// than never caching it, because the offset says something is there.
func TestStoreBodyStructure_RefusesWhatItCannotDecode(t *testing.T) {
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "u", Home: t.TempDir()})
	f, err := idx.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	known := &mailbox.MessageMeta{UID: 1}
	unknown := &mailbox.MessageMeta{UID: 2}
	for _, m := range []*mailbox.MessageMeta{known, unknown} {
		if err := idx.AppendMessage(f.ID, m); err != nil {
			t.Fatal(err)
		}
	}

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	defer fc.Close()

	// A structure the codec round-trips is stored, so the test cannot pass by
	// storeBodyStructure doing nothing at all.
	fc.StoreBodyStructure(known, &imaplib.BodyStructureSinglePart{
		Type: "text", Subtype: "plain", Encoding: "7bit", Size: 3,
	})
	if _, ok := fc.stamps[known.UID]; !ok {
		t.Error("an ordinary body structure was not cached")
	}

	fc.StoreBodyStructure(unknown, foreignBS{})
	if off, stamped := fc.stamps[unknown.UID]; stamped {
		t.Errorf("a structure the codec cannot decode was cached at offset %d; "+
			"every later FETCH would read that offset and miss", off)
	}
}
