package dboxindex_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

func uids(recs []dboxindex.Record) []uint32 {
	out := make([]uint32, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.UID)
	}
	return out
}

func same(a []uint32, b ...uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Applying the tail twice must leave the same mailbox as applying it once.
//
// This is not caution, it is the format. The base is synced up to
// log_file_head_offset, but the records between tail and head have not
// necessarily reached the mailbox, so the reference re-reads from tail. A
// reader that starts there therefore sees changes the base already absorbed --
// and one that applied them blindly would deliver a message twice.
//
// Hand-built rather than taken from a fixture, and deliberately: a base whose
// tail is behind its head exists only between a commit and the sync that
// follows it, so no store captured at rest can carry one. What is asserted is
// behaviour, not layout.
func TestTheOverlapBetweenTailAndHeadIsNotAppliedTwice(t *testing.T) {
	base := []dboxindex.Record{{UID: 1}, {UID: 2}, {UID: 3}}

	for _, tc := range []struct {
		name    string
		changes []dboxindex.Change
		want    []uint32
	}{
		{
			name: "an append the base already carries",
			changes: []dboxindex.Change{
				{Type: dboxindex.Appended, UID: 3},
			},
			want: []uint32{1, 2, 3},
		},
		{
			name: "an expunge of a uid nobody has",
			changes: []dboxindex.Change{
				{Type: dboxindex.Expunged, UID: 4},
			},
			want: []uint32{1, 2, 3},
		},
		{
			name: "the whole overlap, twice over",
			changes: []dboxindex.Change{
				{Type: dboxindex.Appended, UID: 3},
				{Type: dboxindex.Expunged, UID: 4},
				{Type: dboxindex.Appended, UID: 3},
				{Type: dboxindex.Expunged, UID: 4},
			},
			want: []uint32{1, 2, 3},
		},
		{
			name: "a real append still lands",
			changes: []dboxindex.Change{
				{Type: dboxindex.Appended, UID: 3},
				{Type: dboxindex.Appended, UID: 7},
			},
			want: []uint32{1, 2, 3, 7},
		},
		{
			name: "appended and expunged inside one tail",
			changes: []dboxindex.Change{
				{Type: dboxindex.Appended, UID: 8},
				{Type: dboxindex.Expunged, UID: 8},
			},
			want: []uint32{1, 2, 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uids(dboxindex.Apply(base, tc.changes))
			if !same(got, tc.want...) {
				t.Errorf("mailbox is %v, want %v", got, tc.want)
			}
			if !same(uids(base), 1, 2, 3) {
				t.Error("the base was modified in place, so a second pass would start from the wrong state")
			}
		})
	}
}

// The caller's base is not written through.
//
// A slice with spare capacity is the input that distinguishes: append writes
// into the caller's array rather than a new one, so a reader that folds changes
// onto the base it was handed corrupts what its caller still holds. A base with
// no spare capacity hides this entirely, and the one ParseRecords returns has
// none -- which is why this row builds its own.
func TestApplyDoesNotWriteThroughTheCallersBase(t *testing.T) {
	backing := make([]dboxindex.Record, 3, 8)
	backing[0], backing[1], backing[2] = dboxindex.Record{UID: 1}, dboxindex.Record{UID: 2}, dboxindex.Record{UID: 3}
	base := backing[:3]

	got := uids(dboxindex.Apply(base, []dboxindex.Change{{Type: dboxindex.Appended, UID: 9}}))
	if !same(got, 1, 2, 3, 9) {
		t.Fatalf("mailbox is %v, want [1 2 3 9]", got)
	}
	if len(backing) != 3 {
		t.Fatalf("the base slice grew, so this row is not testing what it thinks")
	}
	if backing[:4][3].UID == 9 {
		t.Error("the append landed in the caller's array: a second pass over the same base would start from a mailbox somebody else's changes had already grown")
	}
}

// The same, on the real fixture: base 1,2,3,5 and a tail that appends 6 and
// expunges 5.
func TestApplyingTheReferenceTailGivesTheMailbox(t *testing.T) {
	h, err := dboxindex.ParseHeader(dboxref.IndexBase(t))
	if err != nil {
		t.Fatal(err)
	}
	base, err := dboxindex.ParseRecords(dboxref.IndexBase(t), h)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := dboxindex.ReadChanges(dboxref.IndexLog(t), int(h.LogFileTailOffset))
	if err != nil {
		t.Fatal(err)
	}

	once := uids(dboxindex.Apply(base, changes))
	if !same(once, 1, 2, 3, 6) {
		t.Errorf("mailbox is %v, want [1 2 3 6]", once)
	}
	twice := uids(dboxindex.Apply(dboxindex.Apply(base, changes), changes))
	if !same(twice, once...) {
		t.Errorf("applying the tail twice gives %v and once gives %v", twice, once)
	}
}
