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
	entries, err := dboxindex.ReadMap(raw, int(h.HeaderSize), nil)
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

// An extension id nothing introduced is refused, not skipped.
//
// After a rotation the intros in a new log refer to extensions by the id the
// map's own index holds, having been named in a file that no longer exists.
// Read without that base, those ids resolve to nothing -- and skipping them
// returns a map that is silently short, which is a mailbox whose messages point
// at nothing. There is no fixture for the rotated case yet, so this row builds
// the condition by hand: the same log, read with the intros' names removed.
func TestAnUnknownExtensionIsRefusedRatherThanSkipped(t *testing.T) {
	raw := append([]byte(nil), dboxref.MapLog(t)...)
	h, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		t.Fatal(err)
	}

	// Blank the name_size of every ext-intro, which is what a log written
	// after a rotation looks like: ids only.
	blanked := blankIntroNames(t, raw, int(h.HeaderSize))
	if blanked == 0 {
		t.Fatal("no intros were altered, so this row proves nothing")
	}

	if _, err := dboxindex.ReadMap(raw, int(h.HeaderSize), nil); err == nil {
		t.Error("a log whose extensions nothing introduced was read without complaint, and the map it returns is short by however many records it could not attribute")
	}
}

// blankIntroNames walks the log and sets name_size to zero on every ext-intro,
// leaving the ids. Returns how many it changed.
func blankIntroNames(t *testing.T, b []byte, offset int) int {
	t.Helper()
	const introType = 0x40
	changed := 0
	for pos := offset; pos+8 <= len(b); {
		size := decodeLogSize(b[pos], b[pos+1], b[pos+2], b[pos+3])
		if size < 8 || pos+int(size) > len(b) {
			break
		}
		recType := uint32(b[pos+4]) | uint32(b[pos+5])<<8 | uint32(b[pos+6])<<16 | uint32(b[pos+7])<<24
		if recType&0x0fffffff == introType {
			// name_size is the last uint16 of the fixed part.
			if pos+8+20 <= len(b) {
				b[pos+8+18], b[pos+8+19] = 0, 0
				changed++
			}
		}
		pos += int(size)
	}
	return changed
}

// decodeLogSize mirrors the reader's own unpacking, so the test can walk a log
// without reaching into the package.
func decodeLogSize(b0, b1, b2, b3 byte) uint32 {
	v := uint32(b0)<<24 | uint32(b1)<<16 | uint32(b2)<<8 | uint32(b3)
	if v&0x80808080 != 0x80808080 {
		return 0
	}
	return ((v & 0x0000007f) |
		(v&0x00007f00)>>8<<7 |
		(v&0x007f0000)>>16<<14 |
		(v&0x7f000000)>>24<<21) << 2
}
