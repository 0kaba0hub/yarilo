package maildir

import (
	"bytes"
	"io"
	"sort"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func TestScanReturnsRecordsForDeliveredMessages(t *testing.T) {
	home := t.TempDir()
	mb := New()
	box := mb.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Deliver three messages with different flags so the scan
	// must parse each filename trailer correctly.
	for i, msg := range []struct {
		body  string
		flags []string
	}{
		{"first", []string{`\Seen`}},
		{"second body bytes", nil},
		{"third", []string{`\Seen`, `\Flagged`}},
	} {
		uid := uint32(i + 1)
		_, _, _, err := box.Save("INBOX", io.NopCloser(bytes.NewBufferString(msg.body)),
			uid, int64(len(msg.body)), msg.flags, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	records, err := box.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("scan returned %d records, want 3: %+v", len(records), records)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Filename < records[j].Filename
	})
	for _, r := range records {
		if r.Filename == "" {
			t.Errorf("record has empty filename: %+v", r)
		}
		if r.Size == 0 {
			t.Errorf("record %q has size 0", r.Filename)
		}
		if r.InternalDate.IsZero() {
			t.Errorf("record %q has zero InternalDate", r.Filename)
		}
	}
}

func TestScanEmptyOnMissingFolder(t *testing.T) {
	home := t.TempDir()
	mb := New()
	box := mb.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	// Deliberately skip Init — scan must not blow up on missing dir.
	records, err := box.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("scan returned %d records on bare home, want 0", len(records))
	}
}

// Where the store itself holds keywords, Scan reports them in their own field.
//
// They used to be appended to ScanRecord.Flags, and every consumer that
// forwarded that list on treated the whole of it as flags -- which is how an
// adopted store's keywords were dropped on the way into the index (#1605).
func TestScanReportsKeywordsSeparatelyFromFlags(t *testing.T) {
	home := t.TempDir()
	box := New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home}).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	writeKeywordFile(t, box, "0 $Important\n")
	deliverToCur(t, box, "1700000020.M1P1.host,S=20:2,aS", "From: a@b\r\n\r\nx\r\n")

	recs, err := box.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if len(r.Keywords) != 1 || r.Keywords[0] != "$Important" {
		t.Errorf("Keywords = %v, and the keyword file names $Important on this message", r.Keywords)
	}
	if len(r.Flags) != 1 || r.Flags[0] != `\Seen` {
		t.Errorf("Flags = %v, want only the system flag \\Seen", r.Flags)
	}
}
