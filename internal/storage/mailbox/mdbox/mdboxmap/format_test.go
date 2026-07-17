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
	cases := []struct{ hfid, rebuild uint32 }{
		{0, 0}, {1, 0}, {0xdeadbeef, 0}, {7, 42}, {0xdeadbeef, 0xcafebabe},
	}
	for _, c := range cases {
		raw := encodeMapHeader(c.hfid, c.rebuild)
		if len(raw) != mapHeaderSize {
			t.Fatalf("encoded header %d bytes, want %d", len(raw), mapHeaderSize)
		}
		gotH, gotR := decodeMapHeader(raw)
		if gotH != c.hfid || gotR != c.rebuild {
			t.Errorf("round-trip: got (%d,%d), want (%d,%d)", gotH, gotR, c.hfid, c.rebuild)
		}
	}
	// A legacy 4-byte header (highest_file_id only) reads rebuild_count back as 0.
	legacy := make([]byte, mapHeaderLegacySize)
	binary.LittleEndian.PutUint32(legacy, 99)
	if h, r := decodeMapHeader(legacy); h != 99 || r != 0 {
		t.Errorf("legacy 4-byte header: got (%d,%d), want (99,0)", h, r)
	}
}
