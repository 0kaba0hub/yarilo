package mailindex

import (
	"bytes"
	"errors"
	"testing"
)

func TestComputeRecordLayoutAlignment(t *testing.T) {
	// Mix alignments — widest first wins lowest offset.
	exts := []Extension{
		{Name: "kw", RecordSize: 4, RecordAlign: 4},
		{Name: "ref", RecordSize: 2, RecordAlign: 2},
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
	}
	layout, err := ComputeRecordLayout(exts)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// Expected order in layout.Extensions is by RecordOffset asc.
	// modseq (align 8) at offset 8 (rounded up from 5)
	// kw     (align 4) at offset 16
	// ref    (align 2) at offset 20
	// total record size = 22
	want := map[string]uint16{"modseq": 8, "kw": 16, "ref": 20}
	for _, e := range layout.Extensions {
		if e.RecordOffset != want[e.Name] {
			t.Errorf("ext %q RecordOffset=%d, want %d", e.Name, e.RecordOffset, want[e.Name])
		}
	}
	if layout.RecordSize != 22 {
		t.Errorf("RecordSize=%d, want 22", layout.RecordSize)
	}
}

func TestComputeRecordLayoutHeaderOnlyExtension(t *testing.T) {
	exts := []Extension{
		{Name: "dbox-hdr", HdrSize: 24, HdrData: make([]byte, 24)},
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
	}
	layout, err := ComputeRecordLayout(exts)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	// modseq has Align=8 → bumps offset from 5 to 8 (3 pad bytes),
	// then adds 8 = 16. dbox-hdr adds nothing per-record.
	if layout.RecordSize != 16 {
		t.Errorf("RecordSize=%d, want 16 (5 base + 3 pad + 8 modseq, header-only ext adds zero)", layout.RecordSize)
	}
	for _, e := range layout.Extensions {
		if e.Name == "dbox-hdr" && e.RecordOffset != 0 {
			t.Errorf("dbox-hdr RecordOffset=%d, want 0 (header-only)", e.RecordOffset)
		}
	}
}

func TestComputeRecordLayoutRejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		exts []Extension
	}{
		{"empty name", []Extension{{RecordSize: 4, RecordAlign: 4}}},
		{"name too long", []Extension{{Name: string(make([]byte, ExtNameMaxLength+1)), RecordSize: 4, RecordAlign: 4}}},
		{"zero align with non-zero size", []Extension{{Name: "x", RecordSize: 4}}},
		{"align 3 (not pow2)", []Extension{{Name: "x", RecordSize: 4, RecordAlign: 3}}},
		{"align 16 (>8)", []Extension{{Name: "x", RecordSize: 4, RecordAlign: 16}}},
		{"HdrData length mismatch", []Extension{{Name: "x", HdrSize: 4, HdrData: make([]byte, 2)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ComputeRecordLayout(c.exts)
			if !errors.Is(err, ErrCorrupted) {
				t.Errorf("got %v, want ErrCorrupted", err)
			}
		})
	}
}

func TestExtHeaderRoundTrip(t *testing.T) {
	exts := []Extension{
		{Name: "modseq", HdrSize: 24, HdrData: bytes.Repeat([]byte{0xAB}, 24),
			RecordSize: 8, RecordAlign: 8, ResetID: 0xDEADBEEF},
		{Name: "keywords", HdrSize: 0, RecordSize: 4, RecordAlign: 4, ResetID: 1},
	}
	layout, err := ComputeRecordLayout(exts)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	buf, err := EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(buf) == 0 {
		t.Fatal("encoded buf is empty")
	}
	got, err := DecodeExtHeaders(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(layout.Extensions) {
		t.Fatalf("decoded %d exts, want %d", len(got), len(layout.Extensions))
	}
	// Sort got by name for deterministic compare with layout.Extensions.
	wantByName := map[string]Extension{}
	for _, e := range layout.Extensions {
		wantByName[e.Name] = e
	}
	for _, e := range got {
		w := wantByName[e.Name]
		if e.HdrSize != w.HdrSize {
			t.Errorf("ext %q HdrSize=%d, want %d", e.Name, e.HdrSize, w.HdrSize)
		}
		if e.ResetID != w.ResetID {
			t.Errorf("ext %q ResetID=%x, want %x", e.Name, e.ResetID, w.ResetID)
		}
		if e.RecordOffset != w.RecordOffset {
			t.Errorf("ext %q RecordOffset=%d, want %d", e.Name, e.RecordOffset, w.RecordOffset)
		}
		if e.RecordSize != w.RecordSize {
			t.Errorf("ext %q RecordSize=%d, want %d", e.Name, e.RecordSize, w.RecordSize)
		}
		if e.RecordAlign != w.RecordAlign {
			t.Errorf("ext %q RecordAlign=%d, want %d", e.Name, e.RecordAlign, w.RecordAlign)
		}
		if !bytes.Equal(e.HdrData, w.HdrData) {
			t.Errorf("ext %q HdrData mismatch", e.Name)
		}
	}
}

func TestRecordEncodeDecodeRoundTrip(t *testing.T) {
	exts := []Extension{
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
		{Name: "keywords", RecordSize: 4, RecordAlign: 4},
	}
	layout, err := ComputeRecordLayout(exts)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	rec := Record{
		UID:   42,
		Flags: FlagSeen | FlagFlagged,
		Ext: map[string][]byte{
			"modseq":   {0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
			"keywords": {0xAA, 0xBB, 0xCC, 0xDD},
		},
	}
	buf := make([]byte, layout.RecordSize)
	if err := EncodeRecord(buf, layout, &rec); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeRecord(buf, layout)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UID != rec.UID || got.Flags != rec.Flags {
		t.Errorf("base fields drift: got UID=%d Flags=0x%02x", got.UID, got.Flags)
	}
	for name, want := range rec.Ext {
		if !bytes.Equal(got.Ext[name], want) {
			t.Errorf("ext %q drift: got % x, want % x", name, got.Ext[name], want)
		}
	}
}

func TestRecordEncodeMissingExtIsZeroed(t *testing.T) {
	exts := []Extension{
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
	}
	layout, _ := ComputeRecordLayout(exts)
	rec := Record{UID: 1, Flags: FlagSeen} // no Ext entries
	buf := make([]byte, layout.RecordSize)
	if err := EncodeRecord(buf, layout, &rec); err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 5; i < int(layout.RecordSize); i++ {
		if buf[i] != 0 {
			t.Errorf("byte %d=0x%02x, want 0 (missing ext data must be zeroed)", i, buf[i])
		}
	}
}

func TestRecordEncodeRejectsExtSizeMismatch(t *testing.T) {
	exts := []Extension{
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
	}
	layout, _ := ComputeRecordLayout(exts)
	rec := Record{
		UID:   1,
		Flags: FlagSeen,
		Ext:   map[string][]byte{"modseq": {0x01, 0x02, 0x03}}, // 3 bytes, want 8
	}
	buf := make([]byte, layout.RecordSize)
	err := EncodeRecord(buf, layout, &rec)
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("got %v, want ErrCorrupted", err)
	}
}
