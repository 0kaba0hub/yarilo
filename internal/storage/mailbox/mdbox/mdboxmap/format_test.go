package mdboxmap

import (
	"encoding/binary"
	"testing"
)

func TestEncodeDecodeMapExt(t *testing.T) {
	cases := []struct {
		fileID, offset, size uint32
	}{
		{1, 0, 100},
		{0xffffffff, 0xaaaaaaaa, 0x55555555},
		{0, 0, 0},
	}
	for _, c := range cases {
		raw := encodeMapExt(c.fileID, c.offset, c.size)
		if len(raw) != extMapSize {
			t.Fatalf("encoded len = %d, want %d", len(raw), extMapSize)
		}
		gotFileID, gotOffset, gotSize, err := decodeMapExt(raw)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if gotFileID != c.fileID || gotOffset != c.offset || gotSize != c.size {
			t.Errorf("round-trip: got (%d,%d,%d), want (%d,%d,%d)",
				gotFileID, gotOffset, gotSize, c.fileID, c.offset, c.size)
		}
	}
}

func TestDecodeMapExtRejectsShort(t *testing.T) {
	if _, _, _, err := decodeMapExt([]byte{0, 0, 0}); err == nil {
		t.Fatal("expected error on short buffer")
	}
}

func TestEncodeDecodeRefExt(t *testing.T) {
	for _, want := range []uint16{0, 1, 0xffff} {
		raw := encodeRefExt(want)
		if got := decodeRefExt(raw); got != want {
			t.Errorf("ref round-trip: got %d, want %d", got, want)
		}
	}
	if got := decodeRefExt(nil); got != 0 {
		t.Errorf("nil buf should decode to 0, got %d", got)
	}
}

func TestEncodeDecodeMapHeader(t *testing.T) {
	cases := []struct {
		hfid, rebuild, createFID uint32
		createTime               uint64
	}{
		{0, 0, 0, 0},
		{1, 0, 1, 1_700_000_000},
		{0xdeadbeef, 0, 3, 42},
		{7, 42, 7, 0xcafebabe},
		{0xdeadbeef, 0xcafebabe, 0xdeadbeef, 0x1122334455667788},
	}
	for _, c := range cases {
		raw := encodeMapHeader(c.hfid, c.rebuild, c.createFID, c.createTime)
		if len(raw) != mapHeaderSize {
			t.Fatalf("encoded header %d bytes, want %d", len(raw), mapHeaderSize)
		}
		gotH, gotR, gotFID, gotTS := decodeMapHeader(raw)
		if gotH != c.hfid || gotR != c.rebuild || gotFID != c.createFID || gotTS != c.createTime {
			t.Errorf("round-trip: got (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				gotH, gotR, gotFID, gotTS, c.hfid, c.rebuild, c.createFID, c.createTime)
		}
	}
	// A legacy 4-byte header (highest_file_id only) reads the rest back as 0.
	legacy := make([]byte, mapHeaderLegacySize)
	binary.LittleEndian.PutUint32(legacy, 99)
	if h, r, fid, ts := decodeMapHeader(legacy); h != 99 || r != 0 || fid != 0 || ts != 0 {
		t.Errorf("legacy 4-byte header: got (%d,%d,%d,%d), want (99,0,0,0)", h, r, fid, ts)
	}
	// An 8-byte header (highest_file_id + rebuild_count) reads create fields as 0.
	mid := make([]byte, mapHeaderRebuildSize)
	binary.LittleEndian.PutUint32(mid[0:4], 5)
	binary.LittleEndian.PutUint32(mid[4:8], 9)
	if h, r, fid, ts := decodeMapHeader(mid); h != 5 || r != 9 || fid != 0 || ts != 0 {
		t.Errorf("legacy 8-byte header: got (%d,%d,%d,%d), want (5,9,0,0)", h, r, fid, ts)
	}
}
