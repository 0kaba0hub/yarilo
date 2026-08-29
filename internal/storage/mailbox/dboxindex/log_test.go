package dboxindex_test

import (
	"sort"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// The state of a mailbox is the base plus the log after it, and neither alone.
//
// This fixture is built so that saying so is not a matter of opinion:
//
//	the base counts 1, 2, 3, 5 -- it was written before the last two changes
//	the current log appends 6 and expunges 5
//	the mailbox holds 1, 2, 3, 6, which is what the reference reports
//
// A reader that took the base alone would restore a message the user deleted
// and lose one they have. A reader that took the logs alone would find no
// messages at all: the appends that created 1 to 5 were written to log files
// rotation has since removed.
func TestTheMailboxIsTheBasePlusTheLogAfterIt(t *testing.T) {
	base, err := dboxindex.ParseHeader(dboxref.IndexBase(t))
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	records, err := dboxindex.ParseRecords(dboxref.IndexBase(t), base)
	if err != nil {
		t.Fatalf("parse records: %v", err)
	}

	live := map[uint32]bool{}
	for _, r := range records {
		live[r.UID] = true
	}
	if !live[5] || live[6] {
		t.Fatalf("the base holds %v, and this test needs the one that predates the last changes", keys(live))
	}

	raw := dboxref.IndexLog(t)
	h, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		t.Fatalf("parse log header: %v", err)
	}
	if h.IndexID != base.IndexID {
		t.Fatalf("log belongs to index %d and base to %d: two different stores", h.IndexID, base.IndexID)
	}

	changes, err := dboxindex.ReadChanges(raw, int(base.LogFileTailOffset))
	if err != nil {
		t.Fatalf("read changes: %v", err)
	}
	var appended, expunged int
	for _, c := range changes {
		switch c.Type {
		case dboxindex.Appended:
			live[c.UID] = true
			appended++
		case dboxindex.Expunged:
			delete(live, c.UID)
			expunged++
		}
	}
	if appended != 1 {
		t.Errorf("read %d appends from the tail, want 1", appended)
	}
	// Not one: the reference writes the same expunge twice, once plainly and
	// once carrying a modseq. Counting them would assert its bookkeeping; what
	// matters is that uid 5 is gone.
	if expunged < 1 {
		t.Errorf("read %d expunges from the tail, want at least 1", expunged)
	}

	for _, uid := range []uint32{1, 2, 3, 6} {
		if !live[uid] {
			t.Errorf("uid %d is missing from the reconstructed mailbox", uid)
		}
	}
	if live[5] {
		t.Error("uid 5 survived: the expunge in the tail was not read, and an import would restore a deleted message")
	}
	if len(live) != 4 {
		t.Errorf("reconstructed %d messages (%v), want 4", len(live), keys(live))
	}
}

func keys(m map[uint32]bool) []uint32 {
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// A log this reader does not understand is refused, and a truncated tail is
// stopped at rather than read as data.
func TestALogHeaderIsRefusedRatherThanGuessedAt(t *testing.T) {
	good := dboxref.IndexLog(t)
	for _, tc := range []struct {
		name   string
		mutate func(b []byte)
	}{
		{"another major version", func(b []byte) { b[0] = 2 }},
		{"a header smaller than its fields", func(b []byte) { b[2], b[3] = 8, 0 }},
		{"a header larger than the file", func(b []byte) { b[2], b[3] = 0xff, 0xff }},
		{"an indexid of zero, which marks it corrupt", func(b []byte) { b[4], b[5], b[6], b[7] = 0, 0, 0, 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			tc.mutate(b)
			if _, err := dboxindex.ParseLogHeader(b); err == nil {
				t.Error("accepted, and everything read after it is a guess")
			}
		})
	}
}

// A record whose size marker is absent ends the walk. That is how the reference
// finds the end of what was completely written, and a reader that instead took
// the bytes at face value would read a torn tail as records.
func TestAnUnfinishedTailStopsTheWalk(t *testing.T) {
	raw := append([]byte(nil), dboxref.IndexLog(t)...)
	h, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	full, err := dboxindex.ReadChanges(raw, int(h.HeaderSize))
	if err != nil {
		t.Fatal(err)
	}
	if len(full) == 0 {
		t.Fatal("nothing was read from the whole log, so this row proves nothing")
	}

	// Clear the marker bits on the first record: everything after it is
	// unreachable, exactly as after a crash mid-write.
	raw[int(h.HeaderSize)] = 0
	got, err := dboxindex.ReadChanges(raw, int(h.HeaderSize))
	if err != nil {
		t.Fatalf("a torn tail became an error rather than an end: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %d changes past a record that was never finished", len(got))
	}
}
