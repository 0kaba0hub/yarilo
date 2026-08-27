package dboxv2

import (
	"bytes"
	"testing"
)

func TestMessageHeaderRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		size uint64
	}{
		{"zero", 0},
		{"small", 1234},
		{"max32", 0xffffffff},
		{"max64", 0xffffffffffffffff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := encodeMessageHeader(messageHeader{Size: tc.size})
			if len(raw) != messageHeaderSize {
				t.Fatalf("encoded len = %d, want %d", len(raw), messageHeaderSize)
			}
			h, err := decodeMessageHeader(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if h.Size != tc.size {
				t.Errorf("size = %d, want %d", h.Size, tc.size)
			}
		})
	}
}

func TestMessageHeaderRejectsBadMagic(t *testing.T) {
	raw := encodeMessageHeader(messageHeader{Size: 100})
	raw[0] = 0xff
	if _, err := decodeMessageHeader(raw); err == nil {
		t.Fatal("expected error on bad magic, got nil")
	}
}

func TestMessageHeaderRejectsMissingLF(t *testing.T) {
	raw := encodeMessageHeader(messageHeader{Size: 100})
	raw[len(raw)-1] = ' '
	if _, err := decodeMessageHeader(raw); err == nil {
		t.Fatal("expected error on missing LF, got nil")
	}
}

func TestEncodeFileHeaderLine(t *testing.T) {
	got := string(encodeFileHeaderLine(0x12345678))
	want := "2 M1e C12345678\n"
	if got != want {
		t.Errorf("file header line = %q, want %q", got, want)
	}
}

func TestMetadataBlockRoundTrip(t *testing.T) {
	entries := []metadataEntry{
		{Key: metaKeyGUID, Value: "0123456789abcdef0123456789abcdef"},
		{Key: metaKeyReceived, Value: "1234abcd"},
		{Key: metaKeyVirtualSize, Value: "deadbeef"},
	}
	raw := encodeMetadataBlock(entries)
	if !bytes.HasPrefix(raw, []byte(magicPost)) {
		t.Fatal("metadata block missing magic_post")
	}
	if !bytes.HasSuffix(raw, []byte("\n\n")) {
		t.Fatalf("metadata block missing trailing blank line: %q", raw)
	}
	out, err := decodeMetadataBlock(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(entries) {
		t.Fatalf("got %d entries, want %d", len(out), len(entries))
	}
	for i, e := range entries {
		if out[i].Key != e.Key || out[i].Value != e.Value {
			t.Errorf("entry %d: got %q=%q, want %q=%q", i, out[i].Key, out[i].Value, e.Key, e.Value)
		}
	}
}

func TestMetadataBlockEmpty(t *testing.T) {
	raw := encodeMetadataBlock(nil)
	out, err := decodeMetadataBlock(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d entries, want 0", len(out))
	}
}

func TestGUIDHex(t *testing.T) {
	g := [16]byte{0xde, 0xad, 0xbe, 0xef, 0xfe, 0xed, 0xfa, 0xce, 0xca, 0xfe, 0xba, 0xbe, 0xb0, 0xba, 0xc0, 0xde}
	got := guidHex(g)
	want := "deadbeeffeedfacecafebabeb0bac0de"
	if got != want {
		t.Errorf("guidHex = %q, want %q", got, want)
	}
}
