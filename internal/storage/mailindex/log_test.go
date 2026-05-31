package mailindex

import (
	"bytes"
	"errors"
	"testing"
)

func TestLogHeaderRoundTrip(t *testing.T) {
	h := NewLogHeader(0x12345678, 7, 1717185600)
	h.PrevFileSeq = 6
	h.PrevFileOffset = 4096
	h.InitialModSeq = 0x0123456789abcdef
	var buf bytes.Buffer
	if err := h.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() != LogHeaderSize {
		t.Fatalf("encoded size %d, want %d", buf.Len(), LogHeaderSize)
	}
	got, err := DecodeLogHeader(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MajorVersion != h.MajorVersion ||
		got.MinorVersion != h.MinorVersion ||
		got.IndexID != h.IndexID ||
		got.FileSeq != h.FileSeq ||
		got.PrevFileSeq != h.PrevFileSeq ||
		got.PrevFileOffset != h.PrevFileOffset ||
		got.CreateStamp != h.CreateStamp ||
		got.InitialModSeq != h.InitialModSeq {
		t.Errorf("round-trip drift:\n got:  %+v\n want: %+v", got, h)
	}
}

func TestLogHeaderRejectsWrongMajor(t *testing.T) {
	h := NewLogHeader(1, 1, 1)
	buf := h.EncodeBytes()
	buf[0] = LogMajorVersion + 1
	_, err := DecodeLogHeader(bytes.NewReader(buf))
	if !errors.Is(err, ErrMajorMismatch) {
		t.Errorf("got %v, want ErrMajorMismatch", err)
	}
}

func TestFramedSizeMagicAndRoundTrip(t *testing.T) {
	cases := []uint32{0, 4, 32, 128, 512, 4096, 1 << 20, 1 << 28}
	for _, v := range cases {
		framed, err := EncodeFramedSize(v)
		if err != nil {
			t.Fatalf("EncodeFramedSize(%d): %v", v, err)
		}
		// Every byte of the framed value must have its high bit
		// set — this is the torn-write defence. Test it
		// explicitly so any change to the encoder breaks here.
		for i := 0; i < 4; i++ {
			b := byte((framed >> (8 * i)) & 0xff)
			if b&0x80 == 0 {
				t.Errorf("EncodeFramedSize(%d): byte %d=0x%02x has no high bit", v, i, b)
			}
		}
		got := DecodeFramedSize(framed)
		if got != v {
			t.Errorf("round-trip: EncodeFramedSize(%d) → %#x → DecodeFramedSize → %d", v, framed, got)
		}
	}
}

func TestFramedSizeRejectsUnaligned(t *testing.T) {
	_, err := EncodeFramedSize(7) // not multiple of 4
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted on unaligned input", err)
	}
}

func TestFramedSizeRejectsTooLarge(t *testing.T) {
	_, err := EncodeFramedSize(0x40000000) // = 1 GiB
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted on >= 1 GiB", err)
	}
}

func TestDecodeFramedSizeRejectsTornWrite(t *testing.T) {
	framed, _ := EncodeFramedSize(64)
	// Knock out the magic high bit of byte 0 → torn-write.
	corrupted := framed & 0xffffff7f
	if got := DecodeFramedSize(corrupted); got != 0 {
		t.Errorf("corrupted framed size decoded to %d, want 0 (rejection signal)", got)
	}
}

func TestTxHeaderRoundTrip(t *testing.T) {
	cases := []TxHeader{
		{Size: 8, Type: TxTypeFlags(TxTypeAppend)},
		{Size: 128, Type: TxTypeFlags(TxTypeExpunge) | TxExpungeProt | TxFlagExternal},
		{Size: 4096, Type: TxTypeFlags(TxTypeKeywordUpdate) | TxFlagSync},
	}
	buf := make([]byte, 8)
	for _, c := range cases {
		if err := EncodeTxHeader(buf, c); err != nil {
			t.Fatalf("encode %+v: %v", c, err)
		}
		got, err := DecodeTxHeader(buf)
		if err != nil {
			t.Fatalf("decode %+v: %v", c, err)
		}
		if got != c {
			t.Errorf("round-trip:\n got:  %+v\n want: %+v", got, c)
		}
	}
}

func TestTxTypeFlagsHelpers(t *testing.T) {
	t1 := TxTypeFlags(TxTypeExpunge) | TxExpungeProt | TxFlagExternal
	if t1.Kind() != TxTypeExpunge|TxType(TxExpungeProt) {
		// EXPUNGE_PROT is OR'd into the type byte (within the
		// low 28 bits), not into the flag bits — so Kind() must
		// return TxTypeExpunge | EXPUNGE_PROT bits.
		t.Errorf("Kind() = %#x, want %#x", t1.Kind(), TxTypeExpunge|TxType(TxExpungeProt))
	}
	if !t1.Has(TxFlagExternal) {
		t.Error("Has(External) = false")
	}
	if t1.Has(TxFlagSync) {
		t.Error("Has(Sync) = true unexpectedly")
	}
}

func TestEncodeTxExpungePayloadShape(t *testing.T) {
	payload := EncodeTxExpungePayload([]TxExpunge{
		{UID1: 1, UID2: 3},
		{UID1: 10, UID2: 12},
	})
	want := []byte{
		1, 0, 0, 0, 3, 0, 0, 0,
		10, 0, 0, 0, 12, 0, 0, 0,
	}
	if !bytes.Equal(payload, want) {
		t.Errorf("payload mismatch:\n got:  % x\n want: % x", payload, want)
	}
}

func TestEncodeTxExpungeGUIDPayloadShape(t *testing.T) {
	g := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	payload := EncodeTxExpungeGUIDPayload([]TxExpungeGUID{
		{UID: 5, GUID: g},
	})
	if len(payload) != 20 {
		t.Fatalf("len=%d, want 20", len(payload))
	}
	if payload[0] != 5 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
		t.Errorf("uid bytes: % x", payload[:4])
	}
	if !bytes.Equal(payload[4:], g[:]) {
		t.Errorf("guid bytes: % x", payload[4:])
	}
}

func TestEncodeTxFlagUpdatePayloadShape(t *testing.T) {
	payload := EncodeTxFlagUpdatePayload([]TxFlagUpdate{{
		UID1: 1, UID2: 5,
		AddFlags: FlagSeen, RemoveFlags: FlagFlagged,
		ModSeqIncFlag: 0, Padding: 0,
	}})
	want := []byte{
		1, 0, 0, 0, 5, 0, 0, 0,
		uint8(FlagSeen), uint8(FlagFlagged), 0, 0,
	}
	if !bytes.Equal(payload, want) {
		t.Errorf("payload: % x; want: % x", payload, want)
	}
}

func TestEncodeTxModseqUpdatePayloadShape(t *testing.T) {
	payload := EncodeTxModseqUpdatePayload([]TxModseqUpdate{
		{UID: 1, ModSeqLow32: 0xdeadbeef, ModSeqHigh32: 0x12345678},
	})
	want := []byte{
		1, 0, 0, 0,
		0xef, 0xbe, 0xad, 0xde,
		0x78, 0x56, 0x34, 0x12,
	}
	if !bytes.Equal(payload, want) {
		t.Errorf("payload: % x; want: % x", payload, want)
	}
}

func TestEncodeTxHeaderUpdatePayloadPadding(t *testing.T) {
	// data of 5 bytes → 4 + 5 = 9 → pad 3 to reach 12.
	payload := EncodeTxHeaderUpdatePayload(TxHeaderUpdate{
		Offset: 16, Data: []byte{0xa, 0xb, 0xc, 0xd, 0xe},
	})
	if len(payload) != 12 {
		t.Fatalf("len=%d, want 12 (padded to 4-byte boundary)", len(payload))
	}
	if payload[0] != 16 || payload[1] != 0 || payload[2] != 5 || payload[3] != 0 {
		t.Errorf("header: % x", payload[:4])
	}
	if !bytes.Equal(payload[4:9], []byte{0xa, 0xb, 0xc, 0xd, 0xe}) {
		t.Errorf("data: % x", payload[4:9])
	}
	for i := 9; i < 12; i++ {
		if payload[i] != 0 {
			t.Errorf("padding byte %d=0x%02x, want 0", i, payload[i])
		}
	}
}

func TestEncodeTxExtIntroPayloadShape(t *testing.T) {
	payload := EncodeTxExtIntroPayload(TxExtIntro{
		ExtID:       0xffffffff,
		ResetID:     1,
		HdrSize:     8,
		RecordSize:  12,
		RecordAlign: 4,
		Flags:       TxExtIntroFlagNoShrink,
		Name:        "map",
	})
	if len(payload) != 20+3 {
		t.Fatalf("len=%d, want %d", len(payload), 23)
	}
	// extid LE
	for i, b := range []byte{0xff, 0xff, 0xff, 0xff} {
		if payload[i] != b {
			t.Errorf("extid byte %d: 0x%02x, want 0x%02x", i, payload[i], b)
		}
	}
	if payload[12] != 12 || payload[14] != 4 || payload[16] != 0x01 {
		t.Errorf("record_size/align/flags: % x", payload[12:18])
	}
	if payload[18] != 3 || payload[19] != 0 {
		t.Errorf("name_size: % x", payload[18:20])
	}
	if string(payload[20:23]) != "map" {
		t.Errorf("name=%q, want %q", payload[20:23], "map")
	}
}

func TestEncodeTxExtAtomicIncPayloadShape(t *testing.T) {
	payload := EncodeTxExtAtomicIncPayload([]TxExtAtomicInc{
		{UID: 1, Diff: 1},
		{UID: 2, Diff: -1},
	})
	want := []byte{
		1, 0, 0, 0, 1, 0, 0, 0,
		2, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, // -1 = all-ones
	}
	if !bytes.Equal(payload, want) {
		t.Errorf("payload: % x; want: % x", payload, want)
	}
}

func TestEncodeTxKeywordUpdatePayloadPadding(t *testing.T) {
	// name "x" (1 byte) → header 4 + 1 = 5 → pad 3 → 8 → ranges
	payload := EncodeTxKeywordUpdatePayload(TxKeywordUpdate{
		ModifyType: TxKeywordModifyAdd,
		Name:       "x",
		UIDRanges:  []TxKeywordUIDRange{{UID1: 10, UID2: 20}},
	})
	if len(payload) != 16 {
		t.Fatalf("len=%d, want 16 (4 header + 1 name + 3 pad + 8 range)", len(payload))
	}
	if payload[0] != TxKeywordModifyAdd || payload[2] != 1 {
		t.Errorf("modify/name_size: % x", payload[:4])
	}
	if payload[4] != 'x' {
		t.Errorf("name byte: 0x%02x", payload[4])
	}
	for i := 5; i < 8; i++ {
		if payload[i] != 0 {
			t.Errorf("pad byte %d not zero", i)
		}
	}
	if payload[8] != 10 || payload[12] != 20 {
		t.Errorf("ranges: % x", payload[8:16])
	}
}

func TestEncodeTxAppendPayload(t *testing.T) {
	layout, _ := ComputeRecordLayout([]Extension{
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
	})
	recs := []*Record{
		{UID: 1, Flags: FlagSeen, Ext: map[string][]byte{"modseq": {1, 2, 3, 4, 5, 6, 7, 8}}},
		{UID: 2, Flags: FlagFlagged, Ext: map[string][]byte{"modseq": {9, 8, 7, 6, 5, 4, 3, 2}}},
	}
	payload, err := EncodeTxAppendPayload(layout, recs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if uint32(len(payload)) != layout.RecordSize*uint32(len(recs)) {
		t.Fatalf("len=%d, want %d", len(payload), layout.RecordSize*uint32(len(recs)))
	}
	// Decode each slice back and verify.
	for i, want := range recs {
		got, err := DecodeRecord(payload[i*int(layout.RecordSize):(i+1)*int(layout.RecordSize)], layout)
		if err != nil {
			t.Fatalf("decode rec %d: %v", i, err)
		}
		if got.UID != want.UID || got.Flags != want.Flags {
			t.Errorf("rec %d: got UID=%d Flags=0x%02x", i, got.UID, got.Flags)
		}
		if !bytes.Equal(got.Ext["modseq"], want.Ext["modseq"]) {
			t.Errorf("rec %d modseq drift", i)
		}
	}
}
