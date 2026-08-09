package mdboxmap

import (
	"testing"
)

// The record area is the lookup structure, so the search has to be right at its
// edges: an off-by-one at either end silently reports a live message missing,
// and a missing message is one purge is free to reclaim.
func TestLookupAtTheEdgesOfTheRecordArea(t *testing.T) {
	dir := t.TempDir()
	seedV1(t, dir, v1Entries(), 8, 2, 0) // map_uids 1, 2, 7

	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck

	tests := []struct {
		name string
		uid  uint32
		want bool
	}{
		{"first record", 1, true},
		{"middle record", 2, true},
		{"last record", 7, true},
		{"below the first", 0, false},
		{"between two records", 3, false},
		{"above the last", 9, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, ok, err := m.Lookup(tc.uid)
			if err != nil {
				t.Fatalf("Lookup(%d): %v", tc.uid, err)
			}
			if ok != tc.want {
				t.Fatalf("Lookup(%d) found=%v, want %v", tc.uid, ok, tc.want)
			}
			if ok && e.UID != tc.uid {
				t.Errorf("Lookup(%d) returned the record of map_uid %d", tc.uid, e.UID)
			}
		})
	}
}

// Records are kept in map_uid order because the search depends on it. Appends
// arrive in order and take the cheap tail path; an out-of-order insert is the
// case that would break the ordering if it were appended blindly.
func TestStoreKeepsRecordsOrdered(t *testing.T) {
	var s store
	for _, uid := range []uint32{5, 9, 1, 7} {
		s.insert(MapEntry{UID: uid, Size: uid * 10})
	}
	want := []uint32{1, 5, 7, 9}
	if s.count() != len(want) {
		t.Fatalf("store holds %d records, want %d", s.count(), len(want))
	}
	for i, uid := range want {
		if got := s.at(i); got.UID != uid || got.Size != uid*10 {
			t.Fatalf("record %d is %+v, want map_uid %d", i, got, uid)
		}
	}
	for _, uid := range want {
		if _, ok := s.find(uid); !ok {
			t.Errorf("map_uid %d not findable after ordered insert", uid)
		}
	}
}

// A header that round-trips is the precondition for everything above: the
// record count decides where the record area ends, and the log pairing decides
// how much of the log is replayed on top of it.
func TestBaseHeaderRoundTrips(t *testing.T) {
	want := baseHeader{
		Version:       baseVersion2,
		RecordSize:    baseRecordLen,
		RecordCount:   3,
		NextMapUID:    8,
		HighestFileID: 2,
		RebuildCount:  5,
		CreateFileID:  2,
		CreateTime:    1723200000,
		FoldedOffset:  4096,
		FoldedLineage: 6,
		Lineage:       7,
		RecordsDigest: digestRecords([]byte("records")),
		IndexID:       4242,
	}
	got, err := decodeBaseHeader(encodeBaseHeader(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Errorf("header round-tripped to %+v, want %+v", got, want)
	}
}

// A record that round-trips at a non-zero index proves the offset arithmetic,
// not just the codec.
func TestRecordRoundTripsAtAnOffset(t *testing.T) {
	var s store
	want := MapEntry{UID: 9, FileID: 3, Offset: 1024, Size: 777, RefCount: 2, GUID: [16]byte{0xde, 0xad}}
	s.insert(MapEntry{UID: 1})
	s.insert(want)
	i, ok := s.find(want.UID)
	if !ok {
		t.Fatal("record not found")
	}
	if got := s.at(i); got != want {
		t.Errorf("record round-tripped to %+v, want %+v", got, want)
	}
}
