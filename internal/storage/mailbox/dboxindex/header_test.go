package dboxindex_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// The header of a base index another implementation wrote.
//
// The values are what the store actually holds, recorded when it was captured:
// five messages saved, one expunged, so four remain and the next UID is six.
// A reader with a field at the wrong offset produces numbers that look like
// numbers -- the first attempt at this read the index id as the UID validity
// and got a plausible timestamp -- so the assertions are on values that could
// only come out right if every offset is right.
func TestTheBaseHeaderOfAReferenceIndex(t *testing.T) {
	h, err := dboxindex.ParseHeader(dboxref.IndexBase(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if h.MajorVersion != 7 || h.MinorVersion != 3 {
		t.Errorf("version %d.%d, want 7.3", h.MajorVersion, h.MinorVersion)
	}
	if h.BaseHeaderSize != 120 {
		t.Errorf("base header %d bytes, want 120", h.BaseHeaderSize)
	}
	if h.RecordSize == 0 || h.RecordSize%4 != 0 {
		t.Errorf("record size %d is not a plausible aligned size", h.RecordSize)
	}
	if h.MessagesCount != 4 {
		t.Errorf("messages %d, want 4: five were saved and uid 4 was expunged", h.MessagesCount)
	}
	if h.NextUID != 6 {
		t.Errorf("next uid %d, want 6", h.NextUID)
	}
	if h.UIDValidity == 0 {
		t.Error("uid validity is zero, which a synced mailbox never has")
	}
	// Not asserted: that UIDValidity is read from its own offset rather than
	// from the index id. This fixture cannot tell them apart -- the reference
	// sets both from the same timestamp when it creates the index, so reading
	// one where the other belongs passes every check that can be written here.
	// Telling them apart needs a store whose index has been recreated, which
	// changes the index id and leaves the uid validity alone.
	// The snapshot does not contain everything: the log carries what came after
	// it, and that is the whole reason an importer cannot read the base alone.
	if h.IndexID == 0 {
		t.Error("index id is zero")
	}
	if h.LogFileTailOffset == 0 {
		t.Error("the base claims to be synced to log offset 0, so this fixture cannot show base-plus-log")
	}
}

// A file this reader does not understand is refused rather than read wrongly.
func TestAHeaderIsRefusedRatherThanGuessedAt(t *testing.T) {
	good := dboxref.IndexBase(t)

	for _, tc := range []struct {
		name   string
		mutate func(b []byte)
	}{
		{"another major version", func(b []byte) { b[0] = 8 }},
		{"a base header smaller than its own fields", func(b []byte) { b[2], b[3] = 8, 0 }},
		{"a header larger than the file", func(b []byte) { b[4], b[5], b[6], b[7] = 0xff, 0xff, 0, 0 }},
		{"a zero record size", func(b []byte) { b[8], b[9], b[10], b[11] = 0, 0, 0, 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			tc.mutate(b)
			if _, err := dboxindex.ParseHeader(b); err == nil {
				t.Error("accepted, and every field read from it is then a guess")
			}
		})
	}

	if _, err := dboxindex.ParseHeader(good[:40]); err == nil {
		t.Error("a truncated file was accepted")
	}
}
