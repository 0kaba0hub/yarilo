package file

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stripHdrVsizeExt rewrites the base index at path with the hdr-vsize header
// extension removed, simulating a folder created before the extension existed.
// HeaderSize is recomputed so the stripped file is itself valid.
func stripHdrVsizeExt(t *testing.T, path string) {
	t.Helper()
	mf, err := mailindex.Open(path)
	if err != nil {
		t.Fatalf("open for strip: %v", err)
	}
	kept := mf.Extensions[:0:0]
	for _, e := range mf.Extensions {
		if e.Name != extNameHdrVsize {
			kept = append(kept, e)
		}
	}
	layout, err := mailindex.ComputeRecordLayout(kept)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatalf("encode ext: %v", err)
	}
	mf.Extensions = layout.Extensions
	mf.Layout = layout
	mf.Header.RecordSize = layout.RecordSize
	mf.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	if _, err := mailindex.Recreate(mailindex.RecreateInput{
		Path: path, Header: mf.Header, Extensions: mf.Extensions, Records: mf.Records,
	}); err != nil {
		t.Fatalf("recreate stripped: %v", err)
	}
}

// TestHdrVsizeBackfillRoundTrip is the real regression for #586: a legacy folder
// whose base index lacks the hdr-vsize extension must, after a flush/recalc,
// persist the aggregate to disk — with Recreate accepting the file (HeaderSize
// fixed up). The earlier fix appended the extension without updating HeaderSize,
// so Recreate rejected the file ("header.HeaderSize mismatch"), the base was
// never rewritten (a compaction regression) and the aggregate never persisted.
// This test exercises the full open → flush → reopen path and fails on that bug.
func TestHdrVsizeBackfillRoundTrip(t *testing.T) {
	dir := t.TempDir()

	a := openIdx(dir, testUser)
	fa, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: 1, VSize: 1000, Size: 1000}); err != nil {
		t.Fatal(err)
	}
	base := indexPathFor(a.indexDir("INBOX"))
	a.Close() //nolint:errcheck

	// Make it "legacy": drop the hdr-vsize extension from the persisted base.
	stripHdrVsizeExt(t, base)
	if mf, err := mailindex.Open(base); err != nil {
		t.Fatal(err)
	} else if findExt(mf.Extensions, extNameHdrVsize) != nil {
		t.Fatal("precondition: hdr-vsize should be absent after strip")
	}

	// Reopen and force a flush via recalc — must succeed and persist the ext.
	b := openIdx(dir, testUser)
	fb, err := b.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RecomputeVSize(fb.ID); err != nil {
		t.Fatalf("RecomputeVSize (flush must not fail on a legacy folder): %v", err)
	}
	b.Close() //nolint:errcheck

	// The persisted base must now carry hdr-vsize with the recomputed aggregate.
	mf, err := mailindex.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	ext := findExt(mf.Extensions, extNameHdrVsize)
	if ext == nil {
		t.Fatal("hdr-vsize was not persisted through flush/Recreate")
	}
	v, err := decodeHdrVsize(ext.HdrData)
	if err != nil {
		t.Fatal(err)
	}
	if v.Vsize != 1000 || v.MessageCount != 1 {
		t.Errorf("persisted aggregate = %+v, want {Vsize:1000 MessageCount:1}", v)
	}
}

// TestPersistVsizeBackfillsMissingExt covers #586: a folder whose index predates
// the hdr-vsize extension must still get the aggregate persisted. persistVsizeLocked
// used to no-op when the extension was absent, so the aggregate was never written —
// every quota read full-rescanned and recalc did nothing. It must now backfill the
// extension so the next flush persists it.
func TestPersistVsizeBackfillsMissingExt(t *testing.T) {
	// Legacy index: extensions without hdr-vsize.
	mf, err := mailindex.NewFile(1, []mailindex.Extension{
		{Name: extNameDboxHdr, HdrSize: dboxHdrSize, HdrData: encodeDboxHdr(dboxHdr{}), ResetID: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if findExt(mf.Extensions, extNameHdrVsize) != nil {
		t.Fatal("precondition: index should start without a hdr-vsize extension")
	}

	fs := &folderState{file: mf}
	fs.vsize = hdrVsize{Vsize: 4096, HighestUID: 3, MessageCount: 2}
	fs.persistVsizeLocked()

	ext := findExt(fs.file.Extensions, extNameHdrVsize)
	if ext == nil {
		t.Fatal("hdr-vsize extension was not backfilled")
	}
	got, err := decodeHdrVsize(ext.HdrData)
	if err != nil {
		t.Fatal(err)
	}
	if got.Vsize != 4096 || got.HighestUID != 3 || got.MessageCount != 2 {
		t.Errorf("persisted aggregate = %+v, want {Vsize:4096 HighestUID:3 MessageCount:2}", got)
	}

	// A second persist updates in place — no duplicate extension.
	fs.vsize.Vsize = 8192
	fs.persistVsizeLocked()
	n := 0
	for _, e := range fs.file.Extensions {
		if e.Name == extNameHdrVsize {
			n++
		}
	}
	if n != 1 {
		t.Errorf("hdr-vsize extension count = %d, want 1 (no duplicate)", n)
	}
	if v, _ := decodeHdrVsize(findExt(fs.file.Extensions, extNameHdrVsize).HdrData); v.Vsize != 8192 {
		t.Errorf("in-place update failed: Vsize = %d, want 8192", v.Vsize)
	}
}
