package mdboxmap

import (
	"os"
	"testing"
)

// The base records how far into the log it already reaches, and that pair is
// what makes a crash between writing the base and removing the log survivable.
// Without it the whole log is replayed against a base that already contains it:
// appends are deduplicated by map_uid and look fine, but a refcount delta is a
// delta -- applying it twice moves the count away from the truth, and a count
// too high is a message purge will never reclaim while a count too low is one it
// deletes while a folder still points at it.
func TestRefcountDeltaIsNotDoubleAppliedAfterAFlushCrash(t *testing.T) {
	m, dir := openTestMap(t)
	uid, err := m.AppendRecord(1, 0, 10, [16]byte{1})
	if err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	if err := m.UpdateRefcounts([]uint32{uid}, 2); err != nil {
		t.Fatalf("UpdateRefcounts: %v", err)
	}

	logPath := m.logPath()
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	// AllocFileID folds the log into the base and removes it. Putting the log
	// back is the crash: the base is durable, the truncation never happened.
	if _, err := m.AllocFileID(); err != nil {
		t.Fatalf("AllocFileID: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("the flush did not remove the log: %v", err)
	}
	if err := os.WriteFile(logPath, logBytes, 0o600); err != nil {
		t.Fatalf("restore log: %v", err)
	}
	_ = m.Close()

	again, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close() //nolint:errcheck
	e, ok, err := again.Lookup(uid)
	if err != nil || !ok {
		t.Fatalf("Lookup: ok=%v err=%v", ok, err)
	}
	if e.RefCount != 3 {
		t.Errorf("refcount %d after replaying an already-folded log, want 3", e.RefCount)
	}
}

// The offset is not a licence to skip the tail: whatever a sibling appended
// after the base was written must still be applied.
func TestAppendsPastThePersistedOffsetAreStillReplayed(t *testing.T) {
	m, dir := openTestMap(t)
	if _, err := m.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	if _, err := m.AllocFileID(); err != nil {
		t.Fatalf("AllocFileID: %v", err)
	}

	sibling, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open sibling: %v", err)
	}
	defer sibling.Close() //nolint:errcheck
	uid, err := sibling.AppendRecord(2, 0, 20, [16]byte{2})
	if err != nil {
		t.Fatalf("sibling AppendRecord: %v", err)
	}

	again, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close() //nolint:errcheck
	if _, ok, err := again.Lookup(uid); err != nil || !ok {
		t.Fatalf("an append written after the base was not replayed: ok=%v err=%v", ok, err)
	}
}

// A base rewrite that never reached the rename leaves the previous base in
// place: the temp file is not the map, and the records it was going to hold are
// still in the log.
func TestInterruptedBaseRewriteKeepsThePreviousBase(t *testing.T) {
	m, dir := openTestMap(t)
	uid, err := m.AppendRecord(1, 0, 10, [16]byte{1})
	if err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	if _, err := m.AllocFileID(); err != nil {
		t.Fatalf("AllocFileID: %v", err)
	}
	second, err := m.AppendRecord(2, 0, 20, [16]byte{2})
	if err != nil {
		t.Fatalf("second AppendRecord: %v", err)
	}
	_ = m.Close()

	if err := os.WriteFile(m.path+".tmp", []byte("half-written base"), 0o600); err != nil {
		t.Fatalf("seed tmp: %v", err)
	}

	again, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close() //nolint:errcheck
	for _, want := range []uint32{uid, second} {
		if _, ok, err := again.Lookup(want); err != nil || !ok {
			t.Errorf("map_uid %d lost: ok=%v err=%v", want, ok, err)
		}
	}
}
