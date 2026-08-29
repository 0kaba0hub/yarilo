package dboxv2_test

import (
	"bytes"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
)

// Every record of a file the reference wrote, walked without an index.
//
// This is the recovery path: the records describe themselves, so a store can be
// read when nothing references it. What it cannot recover is in the assertion
// too -- a record names the folder it was first saved to and carries no flags
// at all.
func TestWalkingAReferenceFileFindsEveryRecord(t *testing.T) {
	raw := dboxref.MdboxFile(t)
	r := bytes.NewReader(raw)

	var seen []dboxv2.StoredRecord
	if err := dboxv2.WalkRecords(r, int64(len(raw)), func(rec dboxv2.StoredRecord) error {
		seen = append(seen, rec)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(seen) != 3 {
		t.Fatalf("walked %d records, want 3", len(seen))
	}
	for _, want := range []struct {
		offset int64
		size   int
		folder string
	}{
		{16, 62, "INBOX"},
		{168, 4430, "INBOX"},
		{4690, 67, "Archive"},
	} {
		var found *dboxv2.StoredRecord
		for i := range seen {
			if seen[i].Offset == want.offset {
				found = &seen[i]
			}
		}
		if found == nil {
			t.Errorf("no record at %d", want.offset)
			continue
		}
		if len(found.Body) != want.size {
			t.Errorf("record at %d has a %d-byte body, want %d", want.offset, len(found.Body), want.size)
		}
		if found.OrigMailbox != want.folder {
			t.Errorf("record at %d names folder %q, want %q", want.offset, found.OrigMailbox, want.folder)
		}
		if found.GUID == ([16]byte{}) {
			t.Errorf("record at %d has no guid", want.offset)
		}
		if found.Received.IsZero() {
			t.Errorf("record at %d has no received time", want.offset)
		}
	}
}

// The walk is what says where the next record begins, so a trailer it cannot
// parse must stop it rather than let it resume at a guess.
func TestAWalkStopsOnATrailerItCannotRead(t *testing.T) {
	raw := append([]byte(nil), dboxref.MdboxFile(t)...)

	// The first record's trailer begins after its 30-byte header and 62-byte
	// body; break its magic.
	at := 16 + 30 + 62
	raw[at] = 'x'

	err := dboxv2.WalkRecords(bytes.NewReader(raw), int64(len(raw)), func(dboxv2.StoredRecord) error { return nil })
	if err == nil {
		t.Error("a broken trailer was walked past, and everything after it is read at an offset nobody computed")
	}
}
