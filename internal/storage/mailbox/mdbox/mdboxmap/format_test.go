package mdboxmap

import "testing"

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
	for _, want := range []uint32{0, 1, 0xdeadbeef} {
		raw := encodeMapHeader(want)
		if got := decodeMapHeader(raw); got != want {
			t.Errorf("header round-trip: got %d, want %d", got, want)
		}
	}
}
