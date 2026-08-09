package mdboxmap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// seedV1 writes a v1 base (a mailindex file with per-record extensions) at the
// canonical path, the format every deployed map is in before this change.
func seedV1(t *testing.T, dir string, entries []MapEntry, nextUID, highestFileID, rebuildCount uint32) string {
	t.Helper()
	path := filepath.Join(dir, MapIndexFileName)
	f, err := mailindex.NewFile(4242, defaultExtensions(highestFileID))
	if err != nil {
		t.Fatalf("seed NewFile: %v", err)
	}
	for _, e := range entries {
		f.Records = append(f.Records, &mailindex.Record{
			UID: e.UID,
			Ext: map[string][]byte{
				extMap:  encodeMapExt(e.FileID, e.Offset, e.Size),
				extRef:  encodeRefExt(e.RefCount),
				extGUID: encodeGUIDExt(e.GUID),
			},
		})
	}
	ext := findExt(f.Extensions, extMap)
	ext.HdrData = encodeMapHeader(highestFileID, rebuildCount, 0, 0)
	ext.HdrSize = uint32(len(ext.HdrData))
	layout, err := mailindex.ComputeRecordLayout(f.Extensions)
	if err != nil {
		t.Fatalf("seed layout: %v", err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatalf("seed ext headers: %v", err)
	}
	f.Extensions = layout.Extensions
	f.Layout = layout
	f.Header.RecordSize = layout.RecordSize
	f.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	f.Header.NextUID = nextUID
	f.Header.MessagesCount = uint32(len(f.Records))
	if _, err := mailindex.Recreate(f.ToRecreateInput(path)); err != nil {
		t.Fatalf("seed Recreate: %v", err)
	}
	return path
}

// seedLogAppend writes one TxAppend transaction into a fresh append log, the
// way a writer of either base format does.
func seedLogAppend(t *testing.T, dir string, indexID uint32, entries ...MapEntry) {
	t.Helper()
	path := filepath.Join(dir, MapIndexFileName) + ".log"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("seed log open: %v", err)
	}
	defer f.Close()
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		hdr := mailindex.NewLogHeader(indexID, 1, 1)
		if err := hdr.Encode(f); err != nil {
			t.Fatalf("seed log header: %v", err)
		}
	}
	layout, err := logLayout()
	if err != nil {
		t.Fatalf("seed log layout: %v", err)
	}
	recs := make([]*mailindex.Record, len(entries))
	for i, e := range entries {
		recs[i] = entryToLogRecord(e)
	}
	payload, err := mailindex.EncodeTxAppendPayload(layout, recs)
	if err != nil {
		t.Fatalf("seed log payload: %v", err)
	}
	rec, err := encMapLogRec(mailindex.TxTypeAppend, payload)
	if err != nil {
		t.Fatalf("seed log frame: %v", err)
	}
	if _, err := f.Write(rec); err != nil {
		t.Fatalf("seed log write: %v", err)
	}
}

func v1Entries() []MapEntry {
	return []MapEntry{
		{UID: 1, FileID: 1, Offset: 0, Size: 100, RefCount: 1, GUID: [16]byte{0xa1}},
		{UID: 2, FileID: 1, Offset: 100, Size: 250, RefCount: 3, GUID: [16]byte{0xa2}},
		{UID: 7, FileID: 2, Offset: 0, Size: 40, RefCount: 0, GUID: [16]byte{0xa7}},
	}
}

// The map is not derivable state: map_uid exists only here, and every folder
// index references messages by it. So the transition has exactly one acceptable
// outcome -- the same map_uids, pointing at the same bytes, with the same
// refcounts. Anything else orphans mail.
func TestV1BaseIsConvertedRecordForRecord(t *testing.T) {
	dir := t.TempDir()
	want := v1Entries()
	seedV1(t, dir, want, 8, 2, 5)

	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck

	if got := m.MessageCount(); got != len(want) {
		t.Fatalf("converted map holds %d records, want %d", got, len(want))
	}
	for _, w := range want {
		got, ok, err := m.Lookup(w.UID)
		if err != nil || !ok {
			t.Fatalf("map_uid %d lost in conversion: ok=%v err=%v", w.UID, ok, err)
		}
		if got != w {
			t.Errorf("map_uid %d converted to %+v, want %+v", w.UID, got, w)
		}
	}
	if m.NextMapUID() != 8 {
		t.Errorf("NextMapUID = %d, want 8 (a reused map_uid would point two folders at one message)", m.NextMapUID())
	}
	if m.HighestFileID() != 2 {
		t.Errorf("HighestFileID = %d, want 2", m.HighestFileID())
	}
	if m.RebuildCount() != 5 {
		t.Errorf("RebuildCount = %d, want 5", m.RebuildCount())
	}

	// The file left behind must be v2, and reading it again must not re-convert.
	raw, err := os.ReadFile(filepath.Join(dir, MapIndexFileName))
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if string(raw[0:4]) != baseMagic || raw[4] != baseVersion2 {
		t.Fatalf("base after conversion is not v2: magic=%q version=%d", raw[0:4], raw[4])
	}
}

// A transaction the v1 map had not yet folded into its base is still in the log
// when the converter runs. Losing it would lose a delivery.
func TestConversionKeepsUnfoldedLogRecords(t *testing.T) {
	dir := t.TempDir()
	seedV1(t, dir, v1Entries()[:1], 2, 1, 0)

	// A v1 writer appended one record to the log and never rewrote the base.
	// Written by hand: opening the map through this package would convert it
	// first, and then the log would no longer be the v1 writer's.
	uid := uint32(2)
	seedLogAppend(t, dir, 4242, MapEntry{UID: uid, FileID: 1, Offset: 100, Size: 55, RefCount: 1, GUID: [16]byte{0xb1}})

	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck
	if _, ok, err := m.Lookup(uid); err != nil || !ok {
		t.Fatalf("logged record lost across conversion: ok=%v err=%v", ok, err)
	}
}

// The converter writes to .tmp and renames, so an interruption leaves the old
// file intact and the next open simply converts again -- to the same map_uids.
func TestInterruptedConversionLeavesV1Readable(t *testing.T) {
	dir := t.TempDir()
	want := v1Entries()
	path := seedV1(t, dir, want, 8, 2, 0)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	// Kill between writing the temp file and the rename.
	if err := os.WriteFile(path+".tmp", []byte("half-written v2"), 0o600); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read v1 after crash: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the v1 base was modified before the rename")
	}

	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open after interrupted conversion: %v", err)
	}
	defer m.Close() //nolint:errcheck
	for _, w := range want {
		got, ok, err := m.Lookup(w.UID)
		if err != nil || !ok || got != w {
			t.Errorf("map_uid %d after retry: %+v ok=%v err=%v", w.UID, got, ok, err)
		}
	}
}

// A version this binary does not know is refused, not converted and not
// rebuilt. Rebuilding would mint new map_uids from storage files that do not
// carry them, which detaches every folder record from its message; parsing it as
// v2 would hand out the wrong offsets and let purge unlink the wrong file.
func TestUnknownVersionIsRefused(t *testing.T) {
	dir := t.TempDir()
	seed, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if _, err := seed.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatalf("seed AppendRecord: %v", err)
	}
	_ = seed.Close()

	path := filepath.Join(dir, MapIndexFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	raw[4] = baseVersion2 + 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write base: %v", err)
	}

	m, err := Open(dir, "alice@example.com")
	if err == nil {
		_ = m.Close()
		t.Fatal("a base of an unknown version was opened instead of refused")
	}
	// The file must be untouched: refusal is not a repair.
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read base after refusal: %v", rerr)
	}
	if string(after) != string(raw) {
		t.Error("the refused base was rewritten")
	}
}
