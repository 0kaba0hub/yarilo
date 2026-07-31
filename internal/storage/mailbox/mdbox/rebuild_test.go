package mdbox

import (
	"strconv"
	"strings"
	"testing"
)

func TestScanRecoversAllStoredMessages(t *testing.T) {
	mb, _ := newTestUser(t)
	bodies := []string{"first body", "second one", "third bytes"}
	want := map[string]bool{}
	for i, body := range bodies {
		n, _, err := mb.Save("INBOX", strings.NewReader(body), uint32(i+1), int64(len(body)), nil)
		if err != nil {
			t.Fatal(err)
		}
		want[n] = true
	}

	recs, err := mb.Scan("INBOX")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d scan records, want 3", len(recs))
	}
	gotFilenames := map[string]bool{}
	for _, r := range recs {
		if r.Filename == "" {
			t.Errorf("scan record missing Filename (map_uid): %+v", r)
			continue
		}
		if _, err := strconv.ParseUint(r.Filename, 10, 32); err != nil {
			t.Errorf("filename not a decimal map_uid: %q", r.Filename)
		}
		gotFilenames[r.Filename] = true
		if r.Size == 0 {
			t.Errorf("scan record has zero Size: %+v", r)
		}
		var zero [16]byte
		if r.GUID == zero {
			t.Errorf("scan record missing GUID: %+v", r)
		}
		if r.InternalDate.IsZero() {
			t.Errorf("scan record missing InternalDate: %+v", r)
		}
	}
	for filename := range want {
		if !gotFilenames[filename] {
			t.Errorf("scan dropped filename %q (got %v)", filename, gotFilenames)
		}
	}
}

func TestScanEmptyStorage(t *testing.T) {
	mb, _ := newTestUser(t)
	recs, err := mb.Scan("INBOX")
	if err != nil {
		t.Fatalf("Scan on empty: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("empty storage should yield 0 records, got %d", len(recs))
	}
}

func TestScanAfterPurgeReflectsCompaction(t *testing.T) {
	mb, _ := newTestUser(t)
	var names []string
	for i, body := range []string{"keep", "drop", "keep"} {
		n, _, err := mb.Save("INBOX", strings.NewReader(body), uint32(i+1), int64(len(body)), nil)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := mb.Remove("INBOX", names[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.(*userMailbox).Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	recs, err := mb.Scan("INBOX")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("after purge: %d records, want 2", len(recs))
	}
	// Surviving filenames are unchanged (map_uid preserved).
	got := map[string]bool{}
	for _, r := range recs {
		got[r.Filename] = true
	}
	if !got[names[0]] || !got[names[2]] {
		t.Errorf("kept filenames missing after purge: got %v want %v + %v", got, names[0], names[2])
	}
	if got[names[1]] {
		t.Errorf("expunged filename %q reappeared after purge", names[1])
	}
}
