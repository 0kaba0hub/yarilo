package dboxindex_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

func readMap(t *testing.T) []dboxindex.MapEntry {
	t.Helper()
	raw := dboxref.MapLog(t)
	h, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		t.Fatalf("map log header: %v", err)
	}
	entries, err := dboxindex.ReadMap(raw, int(h.HeaderSize))
	if err != nil {
		t.Fatalf("read map: %v", err)
	}
	return entries
}

// The map of a store that has no map index at all, read from its log.
//
// Seven messages were saved into this store and two of them expunged, and the
// refcounts say which: a map record whose refcount reached zero is a message no
// folder references any more, waiting for a purge to reclaim its bytes. That is
// the pair this fixture can check -- the count, and which two are dead.
func TestTheMapOfAReferenceStore(t *testing.T) {
	entries := readMap(t)

	if len(entries) != 7 {
		t.Fatalf("read %d map records, want 7", len(entries))
	}

	dead := map[uint32]bool{}
	for _, e := range entries {
		if e.FileID != 1 {
			t.Errorf("map uid %d says file %d; this store has one m.1", e.MapUID, e.FileID)
		}
		if e.Size == 0 {
			t.Errorf("map uid %d has size 0 and is not the last record", e.MapUID)
		}
		if e.RefCount == 0 {
			dead[e.MapUID] = true
		}
	}
	if len(dead) != 2 || !dead[4] || !dead[5] {
		t.Errorf("unreferenced records are %v, want exactly 4 and 5 -- the two messages that were expunged", dead)
	}
}

// The offsets point at real records in the store file.
//
// This is what the map is for, and the only assertion that ties the two
// fixtures together: a reader whose offsets are shifted still produces
// plausible numbers, and only the bytes they point at say whether they are
// right. Every live record must begin with the dbox message header magic.
func TestTheMapOffsetsLandOnRecordsInTheStore(t *testing.T) {
	store := dboxref.StoreFile(t)
	for _, e := range readMap(t) {
		if e.RefCount == 0 {
			continue // reclaimed by a purge one day; nothing promises its bytes
		}
		if int(e.Offset)+2 > len(store) {
			t.Errorf("map uid %d points at offset %d in a %d-byte file", e.MapUID, e.Offset, len(store))
			continue
		}
		if got := store[e.Offset : e.Offset+2]; got[0] != 0x01 || got[1] != 0x02 {
			t.Errorf("map uid %d points at %x, which is not the start of a message record", e.MapUID, got)
		}
		if end := int(e.Offset) + int(e.Size); end > len(store) {
			t.Errorf("map uid %d runs to %d, past the %d-byte file", e.MapUID, end, len(store))
		}
	}
}
