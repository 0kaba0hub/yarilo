package dboxv2

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileidx "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestFetchCorruptionClassification: a vanished or truncated message file is
// reported as ErrCorruptStorage (the reactive-rebuild trigger); a healthy file
// reads fine.
func TestFetchCorruptionClassification(t *testing.T) {
	_, mb, _ := newTestUser(t)
	name, err := mb.Save("INBOX", strings.NewReader("hello body\n"), 7, 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc, err := mb.Fetch("INBOX", name, false); err != nil {
		t.Fatalf("healthy fetch: %v", err)
	} else {
		rc.Close()
	}

	// Truncate the file to empty → the file-header read hits EOF → corruption.
	path := filepath.Join(mb.(*userMailbox).folderPath("INBOX"), name)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.Fetch("INBOX", name, false); !errors.Is(err, mailbox.ErrCorruptStorage) {
		t.Fatalf("truncated fetch err = %v, want ErrCorruptStorage", err)
	}

	// Missing file → corruption.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.Fetch("INBOX", name, false); !errors.Is(err, mailbox.ErrCorruptStorage) {
		t.Fatalf("missing fetch err = %v, want ErrCorruptStorage", err)
	}
}

// TestReactiveRebuildDropsVanishedPreservesRest: after a message file vanishes
// out of band, RebuildFolderIndex reconciles the index — the ghost record is
// dropped and every surviving message keeps its UID.
func TestReactiveRebuildDropsVanishedPreservesRest(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for uid := uint32(1); uid <= 3; uid++ {
		n, err := mb.Save("INBOX", strings.NewReader("msg\n"), uid, 4, nil)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
		if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
			UID: uid, Filename: n, Size: 4, VSize: 4,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// UID 2's file vanishes out of band.
	if err := mb.Remove("INBOX", names[1]); err != nil {
		t.Fatal(err)
	}

	rb := mb.(*userMailbox)
	st, err := rb.RebuildFolderIndex(idx, folder)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if st.Scanned != 2 || st.UIDsPreserved != 2 {
		t.Fatalf("stats = %+v, want scanned=2 preserved=2", st)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 2 {
		t.Fatalf("after rebuild: %d messages, want 2", len(msgs))
	}
	gotUIDs := map[uint32]bool{}
	for _, m := range msgs {
		gotUIDs[m.UID] = true
	}
	if !gotUIDs[1] || !gotUIDs[3] || gotUIDs[2] {
		t.Fatalf("surviving UIDs wrong: %v (want 1,3; not 2)", gotUIDs)
	}
}

// TestFsckdMarkerRoundTrip: the FSCKD marker persists across a reopen (proving
// the log-replay path applies header offset 20) and clears on demand.
func TestFsckdMarkerRoundTrip(t *testing.T) {
	_, _, home := newTestUser(t)
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: home}

	a := fileidx.New().OpenUser(info)
	fa, err := a.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if fa.Fsckd {
		t.Fatal("fresh folder should not be flagged")
	}
	if err := a.MarkFolderCorrupt(fa.ID); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()

	// Reopen in a fresh handle: the marker must survive via the persisted log.
	b := fileidx.New().OpenUser(info)
	fb, err := b.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !fb.Fsckd {
		t.Fatal("FSCKD marker did not persist across reopen")
	}
	if err := b.ClearFolderCorrupt(fb.ID); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()

	c := fileidx.New().OpenUser(info)
	fc, err := c.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Fsckd {
		t.Fatal("FSCKD marker not cleared")
	}
	_ = c.Close()
}
