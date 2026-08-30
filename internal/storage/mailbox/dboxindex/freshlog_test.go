package dboxindex_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// A folder whose base index has not been written yet holds its whole state in
// its log, and reading it from the start has to produce that state in full.
//
// The oracle is the reference's own fetch over the folder this fixture came
// from, recorded verbatim in the fixture README:
//
//	uid 1: \Seen
//	uid 2: \Answered
//	uid 3: $Important
//
// Read from log_file_tail_offset instead of from the start -- the base's rule
// applied where there is no base -- and this comes back empty, which is what
// used to happen to every folder of a freshly created store (#1564).
func TestAFolderWithNoBaseIsReadFromItsLog(t *testing.T) {
	raw := dboxref.IndexFreshLog(t)
	h, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		t.Fatalf("log header: %v", err)
	}
	changes, exts, err := dboxindex.ReadChangesAndExtensions(raw, int(h.HeaderSize), nil)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	recs := dboxindex.Apply(nil, changes, nil)

	if len(recs) != 3 {
		t.Fatalf("got %d messages, and the reference reports three", len(recs))
	}
	want := []struct {
		uid     uint32
		flags   uint8
		keyword string
	}{
		{1, 0x08, ""}, // \Seen
		{2, 0x01, ""}, // \Answered
		{3, 0x00, "$Important"},
	}
	for i, w := range want {
		got := recs[i]
		if got.UID != w.uid {
			t.Errorf("record %d has uid %d, want %d", i, got.UID, w.uid)
			continue
		}
		if got.Flags != w.flags {
			t.Errorf("uid %d flags are %#x, want %#x", got.UID, got.Flags, w.flags)
		}
		if w.keyword == "" {
			if len(got.Keywords) != 0 {
				t.Errorf("uid %d keywords are %v, and the reference reports none", got.UID, got.Keywords)
			}
			continue
		}
		if len(got.Keywords) != 1 || got.Keywords[0] != w.keyword {
			t.Errorf("uid %d keywords are %v, want [%s]", got.UID, got.Keywords, w.keyword)
		}
	}

	// The extension that says where a message's bytes are has to come out of
	// the log too. With no base there is nothing else to name it, and an
	// unnamed extension leaves the message unreadable rather than unflagged.
	if _, ok := dboxindex.Find(exts, "mdbox"); !ok {
		var names []string
		for _, e := range exts {
			names = append(names, e.Name)
		}
		t.Errorf("the log introduced %v, and none of them is mdbox", names)
	}
	if len(recs[0].ExtData["mdbox"]) < 8 {
		t.Errorf("uid 1 carries %d bytes of mdbox data; its map uid is not readable",
			len(recs[0].ExtData["mdbox"]))
	}
}
