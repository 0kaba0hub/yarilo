package file

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The reference keeps an mdbox message's storage key in the mailbox index, in a
// per-record "mdbox" extension -- not by scanning the map for a GUID. These are
// its own bytes, so the encoder is checked against them and not against a
// description of them (#1700).
func TestTheMdboxRecordMatchesTheReferenceBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yarilo.index")
	if err := os.WriteFile(path, dboxref.IndexBase(t), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ext := findExt(f.Extensions, extNameMdbox)
	if ext == nil {
		t.Fatalf("their index carries no %q extension: %v", extNameMdbox, extInventory(f.Extensions))
	}
	if ext.RecordSize != mdboxRecSize {
		t.Errorf("their record field is %d bytes, ours %d", ext.RecordSize, mdboxRecSize)
	}
	if len(f.Records) == 0 {
		t.Fatal("their index holds no records")
	}
	for _, rec := range f.Records {
		theirs := rec.Ext[extNameMdbox]
		if len(theirs) != mdboxRecSize {
			t.Fatalf("uid %d carries %d bytes, want %d", rec.UID, len(theirs), mdboxRecSize)
		}
		mapUID := binary.LittleEndian.Uint32(theirs[0:4])
		saveDate := binary.LittleEndian.Uint32(theirs[4:8])
		if mapUID != rec.UID {
			// Their fixture was delivered in order, so the two run together;
			// this is what says the first field is the map uid at all.
			t.Errorf("uid %d names map uid %d", rec.UID, mapUID)
		}
		if got := encodeMdboxRec(mapUID, saveDate); string(got) != string(theirs) {
			t.Errorf("uid %d: we write % x, they wrote % x", rec.UID, got, theirs)
		}
	}
}

// And a message we store carries it: the record, not a sidecar, is where the
// storage key lives -- read back after a reopen, which is the only reading that
// counts (#1700).
func TestOurRecordCarriesTheMapUID(t *testing.T) {
	dir := t.TempDir()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Filename: "7", Size: 10, MapUID: 7, SaveDate: 1788000000,
	}); err != nil {
		t.Fatal(err)
	}
	a.Close() //nolint:errcheck

	b := openIdx(dir, testUser)
	defer b.Close() //nolint:errcheck
	fb, err := b.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	fs := b.folderStateFor(t, "INBOX")
	if len(fs.file.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(fs.file.Records))
	}
	mapUID, saveDate := decodeMdboxRec(fs.file.Records[0].Ext[extNameMdbox])
	if mapUID != 7 || saveDate != 1788000000 {
		t.Errorf("the record says map uid %d saved %d, want 7 and 1788000000", mapUID, saveDate)
	}
	_ = fb
}
