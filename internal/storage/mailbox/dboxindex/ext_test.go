package dboxindex_test

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

func loadBase(t *testing.T) (dboxindex.Header, []dboxindex.Record, []dboxindex.Extension) {
	t.Helper()
	raw := dboxref.IndexBase(t)
	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	recs, err := dboxindex.ParseRecords(raw, h)
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	exts, err := dboxindex.ParseExtensions(raw, h)
	if err != nil {
		t.Fatalf("extensions: %v", err)
	}
	return h, recs, exts
}

// The extension table of a real index, walked rather than assumed.
//
// Each entry carries the length of its own name and header, and the next
// begins after both, padded to eight. A reader with a fixed stride finds the
// first extension and rubbish after it -- which is why the names are asserted,
// not the count.
func TestTheExtensionTableOfAReferenceIndex(t *testing.T) {
	h, _, exts := loadBase(t)

	for _, want := range []string{"keywords", "cache", "vsize", "mdbox", "guid"} {
		e, ok := dboxindex.Find(exts, want)
		if !ok {
			var names []string
			for _, x := range exts {
				names = append(names, x.Name)
			}
			t.Fatalf("no extension named %q; the table reads as %v", want, names)
		}
		if uint32(e.RecordOffset)+uint32(e.RecordSize) > h.RecordSize {
			t.Errorf("%q claims %d..%d of a %d-byte record", e.Name,
				e.RecordOffset, uint32(e.RecordOffset)+uint32(e.RecordSize), h.RecordSize)
		}
	}
}

// The fields that tie a message to its stored bytes.
//
// The values are the reference's own, read out of its dump of this index when
// the fixture was captured: uid 1 is map_uid 1, and its guid is the one below.
// Reading either at a guessed offset produces bytes that look like bytes, so
// the assertion is on the values.
func TestMapUIDAndGUIDOfTheFirstMessage(t *testing.T) {
	_, recs, exts := loadBase(t)
	if len(recs) == 0 {
		t.Fatal("no records")
	}
	first := recs[0]
	if first.UID != 1 {
		t.Fatalf("first record is uid %d, want 1", first.UID)
	}

	mdbox, ok := dboxindex.Find(exts, "mdbox")
	if !ok {
		t.Fatal("no mdbox extension")
	}
	field, ok := dboxindex.FieldIn(first.Raw, mdbox)
	if !ok || len(field) < 4 {
		t.Fatalf("mdbox field is %d bytes", len(field))
	}
	if mapUID := binary.LittleEndian.Uint32(field); mapUID != 1 {
		t.Errorf("map uid %d, want 1: without it a message cannot be tied to its m.<N> record", mapUID)
	}

	guidExt, ok := dboxindex.Find(exts, "guid")
	if !ok {
		t.Fatal("no guid extension")
	}
	g, ok := dboxindex.FieldIn(first.Raw, guidExt)
	if !ok || len(g) != 16 {
		t.Fatalf("guid field is %d bytes, want 16", len(g))
	}
	const want = "fa8916129422936a3d5c0000983eaf89"
	if got := hex.EncodeToString(g); got != want {
		t.Errorf("guid %s, want %s", got, want)
	}
}

// A table this reader cannot walk is refused rather than half-read.
func TestAnExtensionTableIsRefusedRatherThanGuessedAt(t *testing.T) {
	raw := dboxref.IndexBase(t)
	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	base := int(h.BaseHeaderSize)

	for _, tc := range []struct {
		name   string
		mutate func(b []byte)
	}{
		{"a name longer than the header area", func(b []byte) { b[base+14], b[base+15] = 0xff, 0xff }},
		{"a header longer than the header area", func(b []byte) { b[base], b[base+1], b[base+2], b[base+3] = 0xff, 0xff, 0, 0 }},
		{"a record field past the end of a record", func(b []byte) { b[base+8], b[base+9] = 0xff, 0x0f }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), raw...)
			tc.mutate(b)
			if _, err := dboxindex.ParseExtensions(b, h); err == nil {
				t.Error("accepted, and every field read through this table is then a guess")
			}
		})
	}
}

// Keywords, which cannot be read from a record alone.
//
// The names live once in the extension's header and a record carries a bitmask
// over them. The fixture was built so the two are told apart: every message
// carries $HasNoAttachment, which the reference adds itself, and only uid 3
// carries $Important, which was set by hand. A reader that returned every name
// for every record would satisfy a test that only looked for $Important on
// uid 3 -- so uid 1 is asserted not to have it.
func TestKeywordsAreReadFromTheHeaderAndTheRecordTogether(t *testing.T) {
	_, recs, exts := loadBase(t)

	e, ok := dboxindex.Find(exts, "keywords")
	if !ok {
		t.Fatal("no keywords extension")
	}
	names, err := dboxindex.KeywordNames(e)
	if err != nil {
		t.Fatalf("keyword names: %v", err)
	}
	if len(names) != 2 || names[0] != "$HasNoAttachment" || names[1] != "$Important" {
		t.Fatalf("keyword table reads as %v, want [$HasNoAttachment $Important]", names)
	}

	byUID := map[uint32][]string{}
	for _, r := range recs {
		byUID[r.UID] = dboxindex.KeywordsOf(r.Raw, e, names)
	}

	if !has(byUID[3], "$Important") {
		t.Errorf("uid 3 reads as %v, and $Important was set on it", byUID[3])
	}
	if has(byUID[1], "$Important") {
		t.Errorf("uid 1 reads as %v: a keyword nobody set on it, so the mask is not being read", byUID[1])
	}
	for _, uid := range []uint32{1, 2, 3} {
		if !has(byUID[uid], "$HasNoAttachment") {
			t.Errorf("uid %d reads as %v, and the reference sets $HasNoAttachment on all of them", uid, byUID[uid])
		}
	}
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A keyword header that does not describe itself is refused.
func TestAKeywordHeaderIsRefusedRatherThanGuessedAt(t *testing.T) {
	_, _, exts := loadBase(t)
	e, ok := dboxindex.Find(exts, "keywords")
	if !ok {
		t.Fatal("no keywords extension")
	}

	for _, tc := range []struct {
		name   string
		mutate func(b []byte)
	}{
		{"more keywords than the header could hold", func(b []byte) { b[0], b[1] = 0xff, 0xff }},
		{"a name offset past the names", func(b []byte) { b[8], b[9], b[10], b[11] = 0xff, 0xff, 0, 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := e
			bad.HeaderData = append([]byte(nil), e.HeaderData...)
			tc.mutate(bad.HeaderData)
			if _, err := dboxindex.KeywordNames(bad); err == nil {
				t.Error("accepted, and the names read out of it are whatever followed in the file")
			}
		})
	}
}
