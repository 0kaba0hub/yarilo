package dbox

import (
	"bytes"
	"io"
	"sort"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func TestScanReadsGUIDAndSizeFromTrailer(t *testing.T) {
	home := t.TempDir()
	mb := New()
	box := mb.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, body := range []string{"first", "second"} {
		if _, err := box.Save("INBOX", io.NopCloser(bytes.NewBufferString(body)),
			int64(len(body)), nil); err != nil {
			t.Fatalf("save %q: %v", body, err)
		}
	}
	records, err := box.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("scan returned %d records, want 2", len(records))
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Filename < records[j].Filename })
	var zero [16]byte
	for _, r := range records {
		if r.Filename == "" {
			t.Errorf("record has empty filename: %+v", r)
		}
		if r.GUID == zero {
			t.Errorf("record %q has zero GUID; dbox must populate from trailer", r.Filename)
		}
		if r.Size == 0 {
			t.Errorf("record %q has size 0", r.Filename)
		}
		if r.InternalDate.IsZero() {
			t.Errorf("record %q has zero InternalDate", r.Filename)
		}
		if len(r.Flags) != 0 {
			t.Errorf("record %q has flags %v; dbox.Scan must leave flags empty", r.Filename, r.Flags)
		}
	}
}

func TestScanMissingFolderReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	mb := New()
	box := mb.OpenUser(&mailbox.UserInfo{Username: "alice", Home: home})
	records, err := box.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("scan returned %d records on bare home, want 0", len(records))
	}
}
