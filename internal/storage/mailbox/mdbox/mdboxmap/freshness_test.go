package mdboxmap

import (
	"testing"
)

func reloadKind(t *testing.T, kind string) float64 {
	t.Helper()
	c, err := metricMapReload.GetMetricWithLabelValues(kind)
	if err != nil {
		t.Fatalf("get reload counter %q: %v", kind, err)
	}
	return counterValue(t, c)
}

// A freshness check has three outcomes and the base file is read in exactly one
// of them. Which one is decided from the log lineage the base names, not from a
// list of write paths anybody has to keep up to date.

// The base was not rewritten: the tail of the same log is rolled forward into
// the live structure and the file is never opened.
func TestTailOfTheSameLineageIsRolledForward(t *testing.T) {
	m, dir := openTestMap(t)
	if _, err := m.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}

	sibling, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open sibling: %v", err)
	}
	defer sibling.Close() //nolint:errcheck
	uid, err := sibling.AppendRecord(1, 10, 20, [16]byte{2})
	if err != nil {
		t.Fatalf("sibling AppendRecord: %v", err)
	}

	reopens := reloadKind(t, "reopen")
	replays := reloadKind(t, "replay")
	if _, ok, err := m.Lookup(uid); err != nil || !ok {
		t.Fatalf("the sibling's append was not picked up: ok=%v err=%v", ok, err)
	}
	if got := reloadKind(t, "replay"); got == replays {
		t.Error("the tail was applied without being counted as a replay")
	}
	if got := reloadKind(t, "reopen"); got != reopens {
		t.Error("the base was re-read for a log tail, which is the cost this format removes")
	}
}

// The base was rewritten, but only to fold in the log this handle had already
// applied: its records are the ones in memory, so the new header is adopted and
// the record area is not read.
func TestAFoldOfWhatWeAlreadyHaveIsAdoptedWithoutReading(t *testing.T) {
	m, dir := openTestMap(t)
	uid, err := m.AppendRecord(1, 0, 10, [16]byte{1})
	if err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}

	reader, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open reader: %v", err)
	}
	defer reader.Close() //nolint:errcheck
	if _, ok, err := reader.Lookup(uid); err != nil || !ok {
		t.Fatalf("seed Lookup: ok=%v err=%v", ok, err)
	}

	// A file-id allocation folds the log into the base and leaves the records
	// exactly as they were.
	fileID, err := m.AllocFileID()
	if err != nil {
		t.Fatalf("AllocFileID: %v", err)
	}

	// A purge read is one of the paths that must see the current state, so it
	// checks freshness rather than trusting what it already has.
	reopens := reloadKind(t, "reopen")
	folds := reloadKind(t, "fold")
	recs, err := reader.RecordsInFile(1)
	if err != nil || len(recs) != 1 || recs[0].UID != uid {
		t.Fatalf("the record was lost across the fold: %+v err=%v", recs, err)
	}
	if got := reloadKind(t, "fold"); got == folds {
		t.Fatal("a fold of our own state was not recognised as one")
	}
	if got := reloadKind(t, "reopen"); got != reopens {
		t.Error("the base was re-read to learn what memory already held")
	}
	if got := reader.HighestFileID(); got != fileID {
		t.Errorf("HighestFileID = %d after adopting the fold, want %d", got, fileID)
	}
}

// The base was rewritten with different records. Purge, expunge-vanished and the
// refcount recompute all do this while folding the very same log, so offsets
// alone cannot tell this case from the one above -- and getting it wrong leaves
// a reader with refcounts that are not the truth, which is the side where the
// error costs mail.
func TestABaseWrittenPastTheLogForcesAReRead(t *testing.T) {
	m, dir := openTestMap(t)
	uid, err := m.AppendRecord(1, 0, 10, [16]byte{1})
	if err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}

	reader, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open reader: %v", err)
	}
	defer reader.Close() //nolint:errcheck
	seed, err := reader.RecordsInFile(1)
	if err != nil || len(seed) != 1 {
		t.Fatalf("seed RecordsInFile: %+v err=%v", seed, err)
	}
	if seed[0].RefCount != 1 {
		t.Fatalf("seed refcount %d, want 1", seed[0].RefCount)
	}

	// A rebuild recomputes refcounts from the folder references it found and
	// rewrites the base: same log folded, different records.
	if _, err := m.SetRefcountsFromReferences(map[uint32]int{uid: 4}); err != nil {
		t.Fatalf("SetRefcountsFromReferences: %v", err)
	}

	reopens := reloadKind(t, "reopen")
	recs, err := reader.RecordsInFile(1)
	if err != nil || len(recs) != 1 {
		t.Fatalf("RecordsInFile after rebuild: %+v err=%v", recs, err)
	}
	got := recs[0]
	if got.RefCount != 4 {
		t.Errorf("reader sees refcount %d after the base was rewritten, want 4", got.RefCount)
	}
	if reloadKind(t, "reopen") == reopens {
		t.Error("a base holding different records was adopted instead of read")
	}
}
