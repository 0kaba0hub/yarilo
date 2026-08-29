package dboxindex_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// Flag bits, from the reference's own enum.
const (
	flagAnswered = 0x01
	flagFlagged  = 0x02
	flagSeen     = 0x08
)

// mailbox reads the fixture the way an import would: the base's records and
// keywords, then the tail of the current log folded onto them.
func mailbox(t *testing.T) map[uint32]dboxindex.Record {
	t.Helper()
	h, recs, exts := loadBase(t)

	var names []string
	if kw, ok := dboxindex.Find(exts, "keywords"); ok {
		var err error
		names, err = dboxindex.KeywordNames(kw)
		if err != nil {
			t.Fatalf("keyword names: %v", err)
		}
		for i := range recs {
			recs[i].Keywords = dboxindex.KeywordsOf(recs[i].Raw, kw, names)
		}
	}

	changes, err := dboxindex.ReadChanges(dboxref.IndexLog(t), int(h.LogFileTailOffset))
	if err != nil {
		t.Fatalf("read changes: %v", err)
	}
	out := map[uint32]dboxindex.Record{}
	for _, r := range dboxindex.Apply(recs, changes, names) {
		out[r.UID] = r
	}
	return out
}

// The reconstructed mailbox against what the reference reports for it.
//
// These are the values recorded from its own fetch when the fixture was taken:
//
//	uid 1  \Seen        $HasNoAttachment
//	uid 2  \Answered    $HasNoAttachment
//	uid 3               $HasNoAttachment $Important
//	uid 6               $HasNoAttachment
//
// \Recent is not compared: it is a per-session state the reference reports and
// does not store, so an importer neither reads nor carries it.
func TestFlagsAndKeywordsMatchWhatTheReferenceReports(t *testing.T) {
	box := mailbox(t)

	for _, tc := range []struct {
		uid      uint32
		flags    uint8
		keywords []string
	}{
		{1, flagSeen, []string{"$HasNoAttachment"}},
		{2, flagAnswered, []string{"$HasNoAttachment"}},
		{3, 0, []string{"$HasNoAttachment", "$Important"}},
		{6, 0, []string{"$HasNoAttachment"}},
	} {
		r, ok := box[tc.uid]
		if !ok {
			t.Errorf("uid %d is missing from the reconstructed mailbox", tc.uid)
			continue
		}
		if got := r.Flags & (flagAnswered | flagFlagged | flagSeen); got != tc.flags {
			t.Errorf("uid %d flags %#02x, want %#02x", tc.uid, got, tc.flags)
		}
		for _, want := range tc.keywords {
			if !has(r.Keywords, want) {
				t.Errorf("uid %d keywords %v, missing %s", tc.uid, r.Keywords, want)
			}
		}
		if len(r.Keywords) != len(tc.keywords) {
			t.Errorf("uid %d keywords %v, want exactly %v", tc.uid, r.Keywords, tc.keywords)
		}
	}
	if _, ok := box[5]; ok {
		t.Error("uid 5 is still here: it was expunged in the tail")
	}
}

// Seven hundred toggles of one flag must end where they began.
//
// The fixture's tail carries ninety-one flag updates on uid 5 and the rotated
// log another five hundred and sixty-eight, all of them adding and removing
// \Flagged in turn. A reader that treats an update as "set the flags to this"
// rather than as two masks arrives at whichever the last record happened to be
// -- and the last record here is an add, so it would report a flag the mailbox
// does not have.
//
// The rotated log is read from its own start because the base is synced past
// it; what is asserted is the arithmetic, not the mailbox.
func TestAddAndRemoveMasksCancelOut(t *testing.T) {
	raw := dboxref.IndexLogRotated(t)
	h, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := dboxindex.ReadChanges(raw, int(h.HeaderSize))
	if err != nil {
		t.Fatal(err)
	}

	var adds, removes int
	for _, c := range changes {
		if c.Type != dboxindex.FlagsChanged {
			continue
		}
		if c.AddFlags&flagFlagged != 0 {
			adds++
		}
		if c.RemoveFlags&flagFlagged != 0 {
			removes++
		}
	}
	if adds == 0 || removes == 0 {
		t.Fatalf("the log carries %d adds and %d removes of \\Flagged; this row needs both", adds, removes)
	}

	start := []dboxindex.Record{{UID: 5}}
	got := dboxindex.Apply(start, changes, nil)
	if len(got) != 1 {
		t.Fatalf("applying flag updates changed the message list to %d entries", len(got))
	}
	if got[0].Flags&flagFlagged != 0 {
		t.Errorf("\\Flagged is set after %d adds and %d removes", adds, removes)
	}
}

// The two masks are two masks, and they are applied remove-first.
//
// Neither of these can come from the fixture. Its flag updates never carry both
// masks at once and never touch a flag the message already has, so a reader
// that overwrote the flags with the add mask, or applied the masks the other
// way round, passes every row taken from it. The inputs here are chosen to tell
// those apart, which is the only reason they are hand-built.
func TestTheTwoMasksAreMasksAndTheOrderIsRemoveThenAdd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   uint8
		add     uint8
		remove  uint8
		want    uint8
		catches string
	}{
		{
			name:  "an add leaves the flags the message already has",
			start: flagAnswered, add: flagSeen, remove: 0,
			want:    flagAnswered | flagSeen,
			catches: "a reader that sets the flags to the add mask drops \\Answered",
		},
		{
			name:  "replace-everything is remove 0xff and add the wanted ones",
			start: flagAnswered, add: flagSeen, remove: 0xff,
			want:    flagSeen,
			catches: "applying add before remove leaves no flags at all",
		},
		{
			name:  "a flag in both masks ends up set",
			start: 0, add: flagSeen, remove: flagSeen,
			want:    flagSeen,
			catches: "the same, from the other direction",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dboxindex.Apply(
				[]dboxindex.Record{{UID: 1, Flags: tc.start}},
				[]dboxindex.Change{{Type: dboxindex.FlagsChanged, UID: 1, AddFlags: tc.add, RemoveFlags: tc.remove}},
				nil,
			)
			if len(got) != 1 {
				t.Fatalf("got %d records", len(got))
			}
			if got[0].Flags != tc.want {
				t.Errorf("flags %#02x, want %#02x -- %s", got[0].Flags, tc.want, tc.catches)
			}
		})
	}
}
