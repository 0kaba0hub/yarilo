package file

import (
	"testing"

	"github.com/0kaba0hub/yarilo/internal/storage/mailindex"
)

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
