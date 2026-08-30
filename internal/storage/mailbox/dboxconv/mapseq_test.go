package dboxconv_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// The base's tail offset belongs to the log the base was written against, named
// by its sequence number -- not to whichever log is on disk now.
//
// After a rotation the file on disk is a different sequence, and an offset taken
// from the old one lands wherever it happens to land in the new. The reader must
// notice the sequences disagree and read the current log from its start instead
// (#1583).
//
// Built by moving the fixture's own base to a sequence the log does not carry:
// the bytes are the reference's, the disagreement is the thing under test.
func TestAMapBaseFromAnotherLogSequenceIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	base := append([]byte(nil), dboxref.MapBase(t)...)
	logRaw := dboxref.MapBaseLog(t)

	h, err := dboxindex.ParseHeader(base)
	if err != nil {
		t.Fatal(err)
	}
	lh, err := dboxindex.ParseLogHeader(logRaw)
	if err != nil {
		t.Fatal(err)
	}
	if h.LogFileSeq != lh.FileSeq {
		t.Fatalf("the fixture already disagrees: base seq %d, log seq %d", h.LogFileSeq, lh.FileSeq)
	}
	// log_file_seq lives at offset 60 of the header.
	binary.LittleEndian.PutUint32(base[60:], lh.FileSeq+1)

	for name, b := range map[string][]byte{
		"dovecot.map.index":     base,
		"dovecot.map.index.log": logRaw,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := dboxconv.ReadForeignMap(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Everything is still there: the base's records plus the whole log, since
	// the offset that would have skipped part of it was not trusted.
	const want = 760
	if len(got) != want {
		t.Errorf("read %d entries, want %d -- a tail offset from another log was applied to this one",
			len(got), want)
	}
}
