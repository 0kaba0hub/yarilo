package mdboxmap

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTestMap(t *testing.T) (*Map, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, dir
}

func TestOpenCreatesFreshMap(t *testing.T) {
	m, path := openTestMap(t)
	if m.MessageCount() != 0 {
		t.Errorf("fresh map should have 0 records, got %d", m.MessageCount())
	}
	if m.HighestFileID() != 0 {
		t.Errorf("fresh map should have highest_file_id=0, got %d", m.HighestFileID())
	}
	if m.NextMapUID() != 1 {
		t.Errorf("fresh map should have NextMapUID=1, got %d", m.NextMapUID())
	}
	if _, err := Open(path, "alice@example.com"); err != nil {
		t.Errorf("re-Open on fresh file: %v", err)
	}
}

func TestAppendBatchAllocatesUIDs(t *testing.T) {
	m, _ := openTestMap(t)
	b := m.AppendBatch()
	f1, o1 := b.Next(100)
	f2, o2 := b.Next(200)
	if f1 != 1 || o1 != 0 {
		t.Errorf("first Next: got (%d,%d), want (1,0)", f1, o1)
	}
	if f2 != 1 || o2 != 100 {
		t.Errorf("second Next: got (%d,%d), want (1,100)", f2, o2)
	}
	uids, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(uids) != 2 || uids[0] != 1 || uids[1] != 2 {
		t.Errorf("uids = %v, want [1 2]", uids)
	}
	if m.NextMapUID() != 3 {
		t.Errorf("NextMapUID after batch = %d, want 3", m.NextMapUID())
	}
	if m.HighestFileID() != 1 {
		t.Errorf("HighestFileID after batch = %d, want 1", m.HighestFileID())
	}
}

func TestAppendBatchRotatesOnSize(t *testing.T) {
	m, _ := openTestMap(t)
	b := m.AppendBatch()
	huge := defaultRotateSize - 100
	f1, _ := b.Next(huge)
	// Next message would push past the rotate size → file_id rolls.
	f2, o2 := b.Next(200)
	if f1 != 1 {
		t.Errorf("first file_id = %d, want 1", f1)
	}
	if f2 != 2 || o2 != 0 {
		t.Errorf("second after rotation: got (%d,%d), want (2,0)", f2, o2)
	}
	if _, err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if m.HighestFileID() != 2 {
		t.Errorf("HighestFileID after rotation = %d, want 2", m.HighestFileID())
	}
}

func TestLookupRoundTrip(t *testing.T) {
	m, _ := openTestMap(t)
	b := m.AppendBatch()
	b.Next(50)
	b.Next(150)
	uids, _ := b.Finish()

	e, ok, err := m.Lookup(uids[0])
	if err != nil || !ok {
		t.Fatalf("Lookup(%d): ok=%v err=%v", uids[0], ok, err)
	}
	if e.UID != uids[0] || e.FileID != 1 || e.Offset != 0 || e.Size != 50 || e.RefCount != 1 {
		t.Errorf("entry drift: %+v", e)
	}
	e2, ok2, _ := m.Lookup(uids[1])
	if !ok2 || e2.Offset != 50 || e2.Size != 150 {
		t.Errorf("second entry: %+v", e2)
	}
	if _, ok, _ := m.Lookup(9999); ok {
		t.Error("Lookup of missing UID should be ok=false")
	}
}

func TestLookupSurvivesReopen(t *testing.T) {
	m, dir := openTestMap(t)
	b := m.AppendBatch()
	b.Next(123)
	uids, _ := b.Finish()
	_ = m.Close()

	m2, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer m2.Close()
	e, ok, err := m2.Lookup(uids[0])
	if err != nil || !ok {
		t.Fatalf("Lookup after reopen: ok=%v err=%v", ok, err)
	}
	if e.Size != 123 || e.FileID != 1 {
		t.Errorf("entry drift after reopen: %+v", e)
	}
	if m2.NextMapUID() != 2 {
		t.Errorf("NextMapUID after reopen = %d, want 2", m2.NextMapUID())
	}
}

// TestOpenMigratesLegacyFile verifies the one-shot import flow:
// when only the legacy-named file is present at Open() time, we
// rename it in place and the next reopen sees only the
// yarilo-native file.
func TestOpenMigratesLegacyFile(t *testing.T) {
	dir := t.TempDir()

	// Seed: bring up a map under the yarilo name, write a few
	// records, close. Then rename the file back to its legacy
	// name to simulate a fresh import from a canonical tree.
	seed, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	b := seed.AppendBatch()
	b.Next(10)
	b.Next(20)
	want, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	_ = seed.Close()

	yariloPath := filepath.Join(dir, MapIndexFileName)
	legacyPath := filepath.Join(dir, LegacyMapIndexFileName)
	if err := os.Rename(yariloPath, legacyPath); err != nil {
		t.Fatalf("rename to legacy: %v", err)
	}

	// Re-Open: should pick up the legacy file, migrate it, and
	// read every record back.
	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open after legacy seed: %v", err)
	}
	defer m.Close()

	if _, err := os.Stat(yariloPath); err != nil {
		t.Errorf("yarilo-native file not present after migration: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file still on disk after migration: %v", err)
	}
	for _, uid := range want {
		if _, ok, err := m.Lookup(uid); err != nil || !ok {
			t.Errorf("Lookup(%d) after migrate: ok=%v err=%v", uid, ok, err)
		}
	}
}

func TestUpdateRefcounts(t *testing.T) {
	m, _ := openTestMap(t)
	b := m.AppendBatch()
	b.Next(10)
	b.Next(20)
	uids, _ := b.Finish()

	if err := m.UpdateRefcounts(uids, +1); err != nil {
		t.Fatalf("inc: %v", err)
	}
	e, _, _ := m.Lookup(uids[0])
	if e.RefCount != 2 {
		t.Errorf("after +1: refcount = %d, want 2", e.RefCount)
	}
	if err := m.UpdateRefcounts(uids, -2); err != nil {
		t.Fatalf("dec: %v", err)
	}
	e, _, _ = m.Lookup(uids[0])
	if e.RefCount != 0 {
		t.Errorf("after -2: refcount = %d, want 0", e.RefCount)
	}
	// Underflow clamp.
	if err := m.UpdateRefcounts(uids, -1); err != nil {
		t.Fatalf("underflow dec: %v", err)
	}
	e, _, _ = m.Lookup(uids[0])
	if e.RefCount != 0 {
		t.Errorf("underflow not clamped: %d", e.RefCount)
	}
}

func TestUpdateRefcountsRejectsMissingUID(t *testing.T) {
	m, _ := openTestMap(t)
	if err := m.UpdateRefcounts([]uint32{9999}, +1); err == nil {
		t.Fatal("expected error on missing UID, got nil")
	}
}

func TestGetZeroRefFiles(t *testing.T) {
	m, _ := openTestMap(t)
	b := m.AppendBatch()
	b.Next(10)
	b.Next(20)
	uids, _ := b.Finish()

	if files, _ := m.GetZeroRefFiles(); len(files) != 0 {
		t.Errorf("fresh batch has refcount=1, want no zero files; got %v", files)
	}
	_ = m.UpdateRefcounts([]uint32{uids[0]}, -1)
	files, err := m.GetZeroRefFiles()
	if err != nil {
		t.Fatalf("GetZeroRefFiles: %v", err)
	}
	if len(files) != 1 || files[0] != 1 {
		t.Errorf("expected zero-ref in file 1, got %v", files)
	}
}

func TestAppendMoveRewritesAndExpunges(t *testing.T) {
	m, _ := openTestMap(t)
	b := m.AppendBatch()
	b.Next(10)
	b.Next(20)
	b.Next(30)
	uids, _ := b.Finish()

	// uids[1] is going to be expunged; uids[0] and uids[2] get
	// copied to a fresh file_id=2.
	err := m.AppendMove(
		[]MovedRecord{
			{UID: uids[0], FileID: 2, Offset: 0, Size: 10},
			{UID: uids[2], FileID: 2, Offset: 10, Size: 30},
		},
		[]uint32{uids[1]},
	)
	if err != nil {
		t.Fatalf("AppendMove: %v", err)
	}

	if m.MessageCount() != 2 {
		t.Errorf("after move: %d records, want 2", m.MessageCount())
	}
	e0, ok, _ := m.Lookup(uids[0])
	if !ok || e0.FileID != 2 || e0.Offset != 0 {
		t.Errorf("uids[0]: %+v", e0)
	}
	e2, ok, _ := m.Lookup(uids[2])
	if !ok || e2.FileID != 2 || e2.Offset != 10 {
		t.Errorf("uids[2]: %+v", e2)
	}
	if _, ok, _ := m.Lookup(uids[1]); ok {
		t.Error("uids[1] should be expunged")
	}
	if m.HighestFileID() < 2 {
		t.Errorf("HighestFileID = %d, want >= 2", m.HighestFileID())
	}
}

func TestConcurrentAppend(t *testing.T) {
	m, _ := openTestMap(t)
	const N = 20
	var wg sync.WaitGroup
	gotUIDs := make(chan uint32, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := m.AppendBatch()
			b.Next(50)
			uids, err := b.Finish()
			if err != nil {
				t.Errorf("Finish: %v", err)
				return
			}
			gotUIDs <- uids[0]
		}()
	}
	wg.Wait()
	close(gotUIDs)
	seen := make(map[uint32]bool)
	for u := range gotUIDs {
		if seen[u] {
			t.Errorf("duplicate map_uid %d", u)
		}
		seen[u] = true
	}
	if len(seen) != N {
		t.Errorf("got %d distinct uids, want %d", len(seen), N)
	}
}

func TestLookupManyPreservesOrder(t *testing.T) {
	m, _ := openTestMap(t)
	b := m.AppendBatch()
	b.Next(10)
	b.Next(20)
	uids, _ := b.Finish()

	out, err := m.LookupMany([]uint32{uids[1], 9999, uids[0]})
	if err != nil {
		t.Fatalf("LookupMany: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("LookupMany len = %d, want 3", len(out))
	}
	if out[0].UID != uids[1] {
		t.Errorf("slot 0: %+v", out[0])
	}
	if out[1].UID != 0 {
		t.Errorf("slot 1 (missing) should be zero, got %+v", out[1])
	}
	if out[2].UID != uids[0] {
		t.Errorf("slot 2: %+v", out[2])
	}
}

// TestIncrementalLookupPicksUpSiblingAppend is the map-level guard for the #526
// mdbox symptom: a handle that has already cached the map must see a message a
// sibling process delivered — which lands in the append log, not a base rewrite
// — via refresh-on-miss (mirrors Dovecot's mdbox_map_get_seq).
func TestIncrementalLookupPicksUpSiblingAppend(t *testing.T) {
	reader, dir := openTestMap(t)

	// reader caches an empty map.
	if _, ok, _ := reader.Lookup(1); ok {
		t.Fatal("expected empty map")
	}

	// Sibling writer appends (goes to the log incrementally).
	writer, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("sibling Open: %v", err)
	}
	b := writer.AppendBatch()
	b.Next(4096)
	uids, err := b.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	_ = writer.Close()

	// The base index must be untouched — the append is log-only.
	if _, serr := os.Stat(dir + "/" + string(MapIndexFileName) + ".log"); serr != nil {
		t.Fatalf("expected append log to exist: %v", serr)
	}

	// The already-open reader (stale cache) must resolve the sibling's append.
	e, ok, err := reader.Lookup(uids[0])
	if err != nil || !ok {
		t.Fatalf("reader did not pick up sibling append: ok=%v err=%v", ok, err)
	}
	if e.Size != 4096 {
		t.Errorf("entry drift after incremental replay: %+v", e)
	}
}

// TestCreateTimeSurvivesReopen: the per-file create-time recorded for the current
// append file is persisted in the map header and reloaded on reopen, so the
// mdbox_rotate_interval age check keeps working across process restarts.
func TestCreateTimeSurvivesReopen(t *testing.T) {
	m, dir := openTestMap(t)
	const ts int64 = 1_700_000_500
	if err := m.RecordFileCreated(3, ts); err != nil {
		t.Fatalf("RecordFileCreated: %v", err)
	}
	if got, ok := m.CreateTime(3); !ok || got != ts {
		t.Fatalf("CreateTime(3) = (%d,%v), want (%d,true)", got, ok, ts)
	}
	// A non-current file id is unknown.
	if _, ok := m.CreateTime(2); ok {
		t.Errorf("CreateTime(2) should be unknown")
	}
	_ = m.Close()

	m2, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	if got, ok := m2.CreateTime(3); !ok || got != ts {
		t.Errorf("CreateTime(3) after reopen = (%d,%v), want (%d,true)", got, ok, ts)
	}
}
