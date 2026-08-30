package dboxconv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// A map that has a base is read from both halves.
//
// The oracle is the reference's own count over the store this fixture came
// from: 760 messages. The base holds 718 of them and the log the rest, so
// either half alone is short -- and after the log rotates the base is the only
// place the older half exists at all (#1583).
func TestAMapWithABaseIsReadFromBothHalves(t *testing.T) {
	dir := t.TempDir()
	for name, b := range map[string][]byte{
		"dovecot.map.index":     dboxref.MapBase(t),
		"dovecot.map.index.log": dboxref.MapBaseLog(t),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := dboxconv.ReadForeignMap(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	const want = 760
	if len(got) != want {
		t.Fatalf("read %d entries, and the reference reports %d messages", len(got), want)
	}
	// Every one referenced: a record read without its refcount is skipped by the
	// import as a message waiting for a purge, and the folder that names it then
	// cannot be converted at all -- which is the field's failure, one message
	// deep into a corpus of three thousand.
	for _, e := range got {
		if e.RefCount == 0 {
			t.Fatalf("map uid %d reads as unreferenced, and this store has expunged nothing", e.MapUID)
		}
	}

	// The halves, so a failure says which one went missing rather than only
	// that the total is wrong.
	raw := dboxref.MapBase(t)
	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	exts, err := dboxindex.ParseExtensions(raw, h)
	if err != nil {
		t.Fatal(err)
	}
	base, err := dboxindex.ParseMapRecords(raw, h, exts)
	if err != nil {
		t.Fatalf("base records: %v", err)
	}
	if len(base) != 718 {
		t.Errorf("the base holds %d records, and it held 718 when this was captured", len(base))
	}
	tail, err := dboxindex.ReadMap(dboxref.MapBaseLog(t), int(h.LogFileTailOffset), exts)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) >= want {
		t.Errorf("the log past the base's tail holds %d entries; if it held everything this fixture proves nothing", len(tail))
	}
}
