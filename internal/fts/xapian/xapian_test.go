//go:build flatcurve

package xapian

import (
	"path/filepath"
	"testing"
)

// writeDoc opens a fresh writable DB at dir, stores one document with the given
// free-text and boolean terms under docid, commits and closes.
func writeDoc(t *testing.T, dir string, docid uint32, terms, boolTerms []string) {
	t.Helper()
	w, err := OpenWDB(dir)
	if err != nil {
		t.Fatalf("OpenWDB: %v", err)
	}
	doc := NewDoc()
	for _, term := range terms {
		if err := doc.AddTerm(term); err != nil {
			t.Fatalf("AddTerm %q: %v", term, err)
		}
	}
	for _, bt := range boolTerms {
		if err := doc.AddBooleanTerm(bt); err != nil {
			t.Fatalf("AddBooleanTerm %q: %v", bt, err)
		}
	}
	if err := w.ReplaceDocument(docid, doc); err != nil {
		t.Fatalf("ReplaceDocument: %v", err)
	}
	doc.Free()
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	w.Close()
}

func TestWriteSearchRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	writeDoc(t, dir, 1, []string{"alpha", "bravo"}, nil)
	writeDoc(t, dir, 2, []string{"alpha", "charlie"}, nil)

	db, err := OpenDBMulti([]string{dir})
	if err != nil {
		t.Fatalf("OpenDBMulti: %v", err)
	}
	defer db.Close()

	cases := []struct {
		term string
		want []uint32
	}{
		{"alpha", []uint32{1, 2}},
		{"bravo", []uint32{1}},
		{"charlie", []uint32{2}},
		{"missing", nil},
	}
	for _, c := range cases {
		q, err := QueryTerm(c.term)
		if err != nil {
			t.Fatalf("QueryTerm %q: %v", c.term, err)
		}
		hits, err := db.Search(q)
		q.Free()
		if err != nil {
			t.Fatalf("Search %q: %v", c.term, err)
		}
		got := map[uint32]bool{}
		for _, h := range hits {
			got[h.DocID] = true
		}
		if len(got) != len(c.want) {
			t.Errorf("term %q: got %d hits, want %d", c.term, len(got), len(c.want))
		}
		for _, uid := range c.want {
			if !got[uid] {
				t.Errorf("term %q: missing DocID %d", c.term, uid)
			}
		}
	}

	if last, err := db.LastDocID(); err != nil || last != 2 {
		t.Fatalf("LastDocID = %d, %v; want 2", last, err)
	}
	ids, err := db.DocIDs()
	if err != nil || len(ids) != 2 {
		t.Fatalf("DocIDs = %v, %v; want two ids", ids, err)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	w, err := OpenWDB(dir)
	if err != nil {
		t.Fatalf("OpenWDB: %v", err)
	}
	if err := w.SetMetadata("version", "7"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := w.GetMetadata("version")
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if got != "7" {
		t.Errorf("GetMetadata = %q, want %q", got, "7")
	}
	if missing, err := w.GetMetadata("absent"); err != nil || missing != "" {
		t.Errorf("GetMetadata(absent) = %q, %v; want empty", missing, err)
	}
	w.Close()
}

func TestDeleteDocument(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	writeDoc(t, dir, 1, []string{"alpha"}, nil)

	w, err := OpenWDB(dir)
	if err != nil {
		t.Fatalf("OpenWDB: %v", err)
	}
	existed, err := w.DeleteDocument(1)
	if err != nil || !existed {
		t.Fatalf("DeleteDocument(1) existed=%v err=%v; want existed", existed, err)
	}
	again, err := w.DeleteDocument(1)
	if err != nil || again {
		t.Fatalf("DeleteDocument(1) second time existed=%v err=%v; want not existed, no error", again, err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n, err := w.DocCount(); err != nil || n != 0 {
		t.Fatalf("DocCount after delete = %d, %v; want 0", n, err)
	}
	w.Close()
}

func TestQueryCombineAndNot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	writeDoc(t, dir, 1, []string{"shared", "only1"}, nil)
	writeDoc(t, dir, 2, []string{"shared", "only2"}, nil)

	db, err := OpenDBMulti([]string{dir})
	if err != nil {
		t.Fatalf("OpenDBMulti: %v", err)
	}
	defer db.Close()

	// "shared" AND_NOT "only2" → only DocID 1.
	shared, err := QueryTerm("shared")
	if err != nil {
		t.Fatalf("QueryTerm shared: %v", err)
	}
	only2, err := QueryTerm("only2")
	if err != nil {
		t.Fatalf("QueryTerm only2: %v", err)
	}
	q, err := QueryCombine(OpANDNOT, shared, only2)
	if err != nil {
		t.Fatalf("QueryCombine: %v", err)
	}
	hits, err := db.Search(q)
	q.Free()
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].DocID != 1 {
		t.Fatalf("AND_NOT search = %+v; want single DocID 1", hits)
	}
}
