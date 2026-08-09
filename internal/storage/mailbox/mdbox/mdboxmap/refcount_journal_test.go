package mdboxmap

import (
	"os"
	"path/filepath"
	"testing"
)

// seedRecord puts one record in the map and returns its map_uid.
func seedRecord(t *testing.T, m *Map, fileID uint32) uint32 {
	t.Helper()
	uid, err := m.AppendRecord(fileID, 0, 10, [16]byte{byte(fileID)})
	if err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	return uid
}

func refcountOf(t *testing.T, m *Map, uid uint32) uint16 {
	t.Helper()
	e, ok, err := m.Lookup(uid)
	if err != nil || !ok {
		t.Fatalf("Lookup(%d): ok=%v err=%v", uid, ok, err)
	}
	return e.RefCount
}

// A refcount change is a log record, not a rewrite of the whole base: every
// save and every delete changes one, so a rewrite made a full file rewrite the
// price of a single operation (#1205).
func TestRefcountChangeDoesNotRewriteTheBase(t *testing.T) {
	m, dir := openTestMap(t)
	uid := seedRecord(t, m, 1)

	base := filepath.Join(dir, MapIndexFileName)
	before, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}

	if err := m.UpdateRefcounts([]uint32{uid}, +1); err != nil {
		t.Fatalf("AddRefcounts: %v", err)
	}
	after, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Error("the base index was rewritten for a refcount change")
	}
	if got := refcountOf(t, m, uid); got != 2 {
		t.Errorf("refcount = %d, want 2", got)
	}
}

// The count must survive a restart while it still lives in the log: a reader
// that sees only the base sees the old number, and for a refcount the old
// number is a message purge is allowed to delete.
func TestRefcountSurvivesReopenFromTheLog(t *testing.T) {
	m, dir := openTestMap(t)
	uid := seedRecord(t, m, 1)
	if err := m.UpdateRefcounts([]uint32{uid}, +2); err != nil {
		t.Fatalf("AddRefcounts: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close() //nolint:errcheck
	if got := refcountOf(t, again, uid); got != 3 {
		t.Errorf("refcount after reopen = %d, want 3: the log was not replayed", got)
	}
}

// Base and log disagreeing is the ordinary state now, and the log is the later
// truth. Asserted through the reads purge decides from, not through Lookup:
// Lookup answers a cached hit without refreshing, by its own contract, and a
// refcount it reports low is not what deletes a message -- what deletes one is
// the purge scan below.
func TestLogWinsOverTheBaseWherePurgeReads(t *testing.T) {
	m, dir := openTestMap(t)
	uid := seedRecord(t, m, 1)

	// Sibling raises the count through its own handle; the log now holds a
	// number the base does not.
	sibling, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open sibling: %v", err)
	}
	if err := sibling.UpdateRefcounts([]uint32{uid}, +5); err != nil {
		t.Fatalf("sibling AddRefcounts: %v", err)
	}
	if err := sibling.Close(); err != nil {
		t.Fatalf("sibling Close: %v", err)
	}

	recs, err := m.RecordsInFile(1)
	if err != nil {
		t.Fatalf("RecordsInFile: %v", err)
	}
	if len(recs) != 1 || recs[0].RefCount != 6 {
		t.Errorf("records = %+v, want one with refcount 6: the base won over a later log record", recs)
	}
}

// A torn tail is the crash case: the map must replay to the records that were
// written whole, and never to a lower count than those imply.
func TestTornRefcountRecordReplaysToTheWholeOnes(t *testing.T) {
	m, dir := openTestMap(t)
	uid := seedRecord(t, m, 1)
	if err := m.UpdateRefcounts([]uint32{uid}, +1); err != nil {
		t.Fatalf("AddRefcounts: %v", err)
	}
	if err := m.UpdateRefcounts([]uint32{uid}, +1); err != nil {
		t.Fatalf("AddRefcounts: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Cut the last record in half, as an interrupted write would leave it.
	logPath := filepath.Join(dir, MapIndexFileName) + ".log"
	st, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if err := os.Truncate(logPath, st.Size()-4); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	again, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close() //nolint:errcheck
	// The first increment was written whole and must survive; the torn one may
	// or may not, but the count may never fall below what whole records say.
	if got := refcountOf(t, again, uid); got < 2 {
		t.Errorf("refcount = %d, want at least 2: a whole record was lost with the torn one", got)
	}
}

// Purge decides what to delete from these answers, so they must see the log.
// Reading the base alone offers a file whose count was raised moments ago --
// which is a message deleted while still referenced.
func TestPurgeReadsSeeTheLog(t *testing.T) {
	m, dir := openTestMap(t)
	uid := seedRecord(t, m, 7)
	// Drop it to zero: the file is now a purge candidate.
	if err := m.UpdateRefcounts([]uint32{uid}, -1); err != nil {
		t.Fatalf("AddRefcounts: %v", err)
	}
	files, err := m.GetZeroRefFiles()
	if err != nil {
		t.Fatalf("GetZeroRefFiles: %v", err)
	}
	if len(files) != 1 || files[0] != 7 {
		t.Fatalf("zero-ref files = %v, want [7]", files)
	}

	// A sibling references it again. The base still says zero.
	sibling, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open sibling: %v", err)
	}
	defer sibling.Close() //nolint:errcheck
	if err := sibling.UpdateRefcounts([]uint32{uid}, +1); err != nil {
		t.Fatalf("sibling AddRefcounts: %v", err)
	}

	files, err = m.GetZeroRefFiles()
	if err != nil {
		t.Fatalf("GetZeroRefFiles: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("zero-ref files = %v, want none: purge would delete a referenced message", files)
	}

	recs, err := m.RecordsInFile(7)
	if err != nil {
		t.Fatalf("RecordsInFile: %v", err)
	}
	if len(recs) != 1 || recs[0].RefCount != 1 {
		t.Errorf("records = %+v, want one with refcount 1", recs)
	}
}
