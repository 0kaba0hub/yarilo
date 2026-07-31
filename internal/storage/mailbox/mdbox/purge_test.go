package mdbox

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPurgeNoOpWhenNoZeroRef(t *testing.T) {
	mb, _ := newTestUser(t)
	for i := 0; i < 3; i++ {
		if _, _, err := mb.Save("INBOX", strings.NewReader("body"), uint32(i+1), 4, nil); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := mb.(*userMailbox).Purge()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if stats.FilesScanned != 0 || stats.RecordsKept != 0 || stats.RecordsExpunged != 0 {
		t.Errorf("expected no work, got %+v", stats)
	}
}

func TestPurgeUnlinksAllZeroFile(t *testing.T) {
	mb, home := newTestUser(t)
	var names []string
	for i := 0; i < 3; i++ {
		n, _, err := mb.Save("INBOX", strings.NewReader("body"), uint32(i+1), 4, nil)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	// Remove all → refcounts all hit zero.
	for _, n := range names {
		if err := mb.Remove("INBOX", n); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := mb.(*userMailbox).Purge()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if stats.FilesUnlinked != 1 || stats.RecordsExpunged != 3 {
		t.Errorf("unexpected stats: %+v", stats)
	}
	// m.1 must be gone.
	if _, err := os.Stat(filepath.Join(home, "mdbox", "storage", "m.1")); !os.IsNotExist(err) {
		t.Errorf("m.1 still on disk after all-zero purge: %v", err)
	}
}

func TestPurgeCompactsLiveRecords(t *testing.T) {
	mb, home := newTestUser(t)
	var names []string
	for i, body := range []string{"keep one", "DROP this body", "keep two"} {
		n, _, err := mb.Save("INBOX", strings.NewReader(body), uint32(i+1), int64(len(body)), nil)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	// Drop the middle one.
	if err := mb.Remove("INBOX", names[1]); err != nil {
		t.Fatal(err)
	}
	oldSize, _ := mb.(*userMailbox).fileSize(filepath.Join(home, "mdbox", "storage", "m.1"))

	stats, err := mb.(*userMailbox).Purge()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if stats.FilesRewritten != 1 {
		t.Errorf("expected 1 rewrite, got %+v", stats)
	}
	if stats.RecordsKept != 2 || stats.RecordsExpunged != 1 {
		t.Errorf("partition wrong: %+v", stats)
	}
	if stats.BytesReclaimed <= 0 {
		t.Errorf("expected positive BytesReclaimed, got %d (old m.1 size = %d)", stats.BytesReclaimed, oldSize)
	}

	// Old m.1 gone, new m.2 (allocated by AllocFileID) exists.
	if _, err := os.Stat(filepath.Join(home, "mdbox", "storage", "m.1")); !os.IsNotExist(err) {
		t.Errorf("m.1 still on disk after compact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "mdbox", "storage", "m.2")); err != nil {
		t.Errorf("m.2 should exist after compact: %v", err)
	}

	// Surviving filenames still Fetch to the original bodies —
	// map_uid is preserved across the move.
	rc, err := mb.Fetch("INBOX", names[0], false)
	if err != nil {
		t.Fatalf("Fetch kept[0]: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "keep one" {
		t.Errorf("body drift on kept[0]: %q", got)
	}
	rc, err = mb.Fetch("INBOX", names[2], false)
	if err != nil {
		t.Fatalf("Fetch kept[2]: %v", err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "keep two" {
		t.Errorf("body drift on kept[2]: %q", got)
	}

	// Expunged map_uid must be gone.
	if _, err := mb.Fetch("INBOX", names[1], false); err == nil {
		t.Error("Fetch of expunged uid should fail")
	}
}

func TestPurgeIdempotent(t *testing.T) {
	mb, _ := newTestUser(t)
	n, _, _ := mb.Save("INBOX", strings.NewReader("body"), 1, 4, nil)
	_ = mb.Remove("INBOX", n)
	if _, err := mb.(*userMailbox).Purge(); err != nil {
		t.Fatalf("first Purge: %v", err)
	}
	stats, err := mb.(*userMailbox).Purge()
	if err != nil {
		t.Fatalf("second Purge: %v", err)
	}
	if stats.FilesScanned != 0 {
		t.Errorf("second Purge should be no-op, got %+v", stats)
	}
}
