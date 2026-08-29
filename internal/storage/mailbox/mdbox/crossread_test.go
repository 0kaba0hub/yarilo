package mdbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
)

// The reader the importer uses and the reader this driver uses must agree.
//
// The import in #1524 cannot go through this driver: it walks a store whose map
// is somebody else's, so it reads records by offset with dboxv2.ReadRecordBodyAt
// instead. That is a second way into the same bytes, and a second way into the
// same bytes is how a format quietly forks -- one of them gains a fix and the
// other does not.
//
// So they are held against each other on a file the reference wrote, at every
// record in it, including the two that no file-header line precedes.
func TestTheImporterReadsRecordsTheSameWayTheDriverDoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.1")
	if err := os.WriteFile(path, dboxref.MdboxFile(t), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck

	hdrSize, err := dboxv2.FileHeaderSize(f)
	if err != nil {
		t.Fatalf("file header size: %v", err)
	}

	for _, offset := range []uint32{16, 168, 4690} {
		ours, err := readRecordBody(f, offset)
		if err != nil {
			t.Fatalf("driver read at %d: %v", offset, err)
		}
		theirs, err := dboxv2.ReadRecordBodyAt(f, int64(offset), hdrSize)
		if err != nil {
			t.Fatalf("importer read at %d: %v", offset, err)
		}
		if !bytes.Equal(ours, theirs) {
			t.Errorf("record at %d: the driver reads %d bytes and the importer %d", offset, len(ours), len(theirs))
		}
	}
}
