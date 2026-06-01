package mdboxmap

import (
	"path/filepath"
	"sync"
	"testing"
)

func openTestMap(t *testing.T) (*Map, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dovecot.map.index")
	m, err := Open(path, "alice@example.com")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, path
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
	huge := rotateSize - 100
	f1, _ := b.Next(huge)
	// Next message would push past rotateSize → file_id rolls.
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
	m, path := openTestMap(t)
	b := m.AppendBatch()
	b.Next(123)
	uids, _ := b.Finish()
	_ = m.Close()

	m2, err := Open(path, "alice@example.com")
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
