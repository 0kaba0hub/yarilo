package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stageLegacyFolder builds a folder as the pre-fix binary left it: bodies in
// storage, and an index whose records carry no GUID because the extension did
// not exist yet. The extension is stripped from the on-disk file afterwards so
// this is the real pre-upgrade shape, not a fresh index with empty fields.
func stageLegacyFolder(t *testing.T, n int) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "backfill@example.com", Home: home}
	mb := maildir.New().OpenUser(user)
	idx := file.New().OpenUser(user)
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	for i := 0; i < n; i++ {
		uid, err := idx.AllocateUID(folder.ID)
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		body := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
		name, vsize, _, err := mb.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		// GUID deliberately omitted: this is the pre-fix record shape.
		if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
			UID: uid, Filename: name, Size: uint32(len(body)), VSize: vsize,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	stripGUIDExtension(t, home)

	idx2 := file.New().OpenUser(user)
	t.Cleanup(func() { _ = idx2.Close() })
	folder2, err := idx2.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("reopen folder: %v", err)
	}
	return mb, idx2, folder2
}

// stripGUIDExtension rewrites every index under root without the guid
// extension, reproducing a file written before the extension existed.
func stripGUIDExtension(t *testing.T, root string) {
	t.Helper()
	found := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != file.IndexFileName {
			return nil
		}
		f, err := mailindex.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		var exts []mailindex.Extension
		for _, e := range f.Extensions {
			if e.Name != "guid" {
				exts = append(exts, e)
			}
		}
		if len(exts) == len(f.Extensions) {
			return nil
		}
		layout, err := mailindex.ComputeRecordLayout(exts)
		if err != nil {
			return fmt.Errorf("layout: %w", err)
		}
		extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
		if err != nil {
			return fmt.Errorf("encode ext: %w", err)
		}
		for _, rec := range f.Records {
			delete(rec.Ext, "guid")
		}
		f.Extensions = layout.Extensions
		f.Layout = layout
		f.Header.RecordSize = layout.RecordSize
		f.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
		if _, err := mailindex.Recreate(f.ToRecreateInput(path)); err != nil {
			return fmt.Errorf("recreate: %w", err)
		}
		found++
		return nil
	})
	if err != nil {
		t.Fatalf("strip guid extension: %v", err)
	}
	if found == 0 {
		t.Fatal("no index file found to downgrade")
	}
}

func guidsByUID(t *testing.T, idx mailbox.UserIndex, folderID uint64) map[uint32][16]byte {
	t.Helper()
	msgs, err := idx.GetMessages(folderID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	out := make(map[uint32][16]byte, len(msgs))
	for _, m := range msgs {
		out[m.UID] = m.GUID
	}
	return out
}

// TestBackfillStampsLegacyRecords is the acceptance case: mail that was on disk
// before the upgrade must end up with a real EMAILID.
func TestBackfillStampsLegacyRecords(t *testing.T) {
	var zero [16]byte
	mb, idx, folder := stageLegacyFolder(t, 3)

	before := guidsByUID(t, idx, folder.ID)
	for uid, g := range before {
		if g != zero {
			t.Fatalf("staged record uid=%d already has GUID %x, test is not exercising the legacy shape", uid, g)
		}
	}
	need, err := idx.GUIDBackfillNeeded(folder.ID)
	if err != nil {
		t.Fatalf("needed: %v", err)
	}
	if !need {
		t.Fatal("legacy folder reported as already backfilled")
	}

	if err := idxrebuild.BackfillGUIDs(mb, idx, folder, "INBOX"); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	after := guidsByUID(t, idx, folder.ID)
	seen := map[[16]byte]uint32{}
	for uid, g := range after {
		if g == zero {
			t.Errorf("uid=%d still has a zero GUID after backfill", uid)
		}
		if prev, dup := seen[g]; dup {
			t.Errorf("uid=%d and uid=%d share GUID %x", prev, uid, g)
		}
		seen[g] = uid
	}

	// The marker must flip, or every open pays the pass again.
	need, err = idx.GUIDBackfillNeeded(folder.ID)
	if err != nil {
		t.Fatalf("needed after: %v", err)
	}
	if need {
		t.Error("folder still reported as pending after a completed backfill")
	}
}

// TestBackfillIsIdempotent covers the crash-safety requirement: an aborted pass
// leaves some records stamped, and the resumed run must reach the same state
// without touching what was already assigned.
func TestBackfillIsIdempotent(t *testing.T) {
	mb, idx, folder := stageLegacyFolder(t, 4)

	// Simulate a pass that was killed after writing one record.
	partial := guidsByUID(t, idx, folder.ID)
	var firstUID uint32
	for uid := range partial {
		if firstUID == 0 || uid < firstUID {
			firstUID = uid
		}
	}
	pinned := [16]byte{0xAA, 0xBB, 0xCC}
	if err := idx.SetGUIDs(folder.ID, map[uint32][16]byte{firstUID: pinned}); err != nil {
		t.Fatalf("partial pass: %v", err)
	}

	if err := idxrebuild.BackfillGUIDs(mb, idx, folder, "INBOX"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	first := guidsByUID(t, idx, folder.ID)
	if first[firstUID] != pinned {
		t.Errorf("resumed pass rewrote an assigned GUID: %x, want %x", first[firstUID], pinned)
	}

	if err := idxrebuild.BackfillGUIDs(mb, idx, folder, "INBOX"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second := guidsByUID(t, idx, folder.ID)
	if len(first) != len(second) {
		t.Fatalf("message count changed: %d -> %d", len(first), len(second))
	}
	for uid, g := range first {
		if second[uid] != g {
			t.Errorf("uid=%d GUID changed on a repeat run: %x -> %x", uid, g, second[uid])
		}
	}
}

// TestBackfillMatchesStorage guards the "transfer, not generation" rule: the
// index must end up holding exactly what the driver reports, or a later rebuild
// from storage would change EMAILID.
func TestBackfillMatchesStorage(t *testing.T) {
	mb, idx, folder := stageLegacyFolder(t, 3)
	if err := idxrebuild.BackfillGUIDs(mb, idx, folder, "INBOX"); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	scanned, err := mb.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	fromStorage := make(map[string][16]byte, len(scanned))
	for _, r := range scanned {
		fromStorage[r.Filename] = r.GUID
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	for _, m := range msgs {
		want, ok := fromStorage[m.Filename]
		if !ok {
			t.Errorf("storage does not report %q", m.Filename)
			continue
		}
		if m.GUID != want {
			t.Errorf("index GUID %x != storage GUID %x for %q", m.GUID, want, m.Filename)
		}
	}
}
