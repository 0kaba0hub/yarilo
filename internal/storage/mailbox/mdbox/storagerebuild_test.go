package mdbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// newBoxAndIndex wires a real mdbox box and a real file index over the same
// home, so the storage-wide rebuild can be exercised end to end.
func newBoxAndIndex(t *testing.T, home string) (*userMailbox, mailbox.UserIndex) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@x.io", Home: home}
	box := New().OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatalf("box init: %v", err)
	}
	idx := file.New().OpenUser(info)
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open INBOX index: %v", err)
	}
	return box, idx
}

func deliverMsg(t *testing.T, box *userMailbox, idx mailbox.UserIndex, folder, body string) uint32 {
	t.Helper()
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		t.Fatalf("open %s: %v", folder, err)
	}
	uid, err := idx.AllocateUID(f.ID)
	if err != nil {
		t.Fatalf("alloc uid: %v", err)
	}
	fn, err := box.Save(folder, strings.NewReader(body), uid, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: fn, Size: uint32(len(body))}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return uid
}

func folderCount(t *testing.T, idx mailbox.UserIndex, folder string) int {
	t.Helper()
	f, err := idx.OpenFolder(folder, 0)
	if err != nil {
		t.Fatalf("open %s: %v", folder, err)
	}
	msgs, err := idx.GetMessages(f.ID, allMessages)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	return len(msgs)
}

// TestRebuildAdoptsOrphanIntoInbox: a message saved to storage (map record) but
// referenced by no folder index is adopted into INBOX.
func TestRebuildAdoptsOrphanIntoInbox(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "referenced message body\r\n")

	// Orphan: written to storage, never appended to any folder index.
	if _, err := box.Save("INBOX", strings.NewReader("orphan body\r\n"), 0, 12, nil); err != nil {
		t.Fatalf("save orphan: %v", err)
	}

	stats, err := box.RebuildStorage(idx)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.OrphansAdopted != 1 {
		t.Errorf("orphans adopted = %d, want 1", stats.OrphansAdopted)
	}
	if got := folderCount(t, idx, "INBOX"); got != 2 {
		t.Errorf("INBOX count = %d, want 2 (1 referenced + 1 adopted)", got)
	}
}

// TestRebuildDropsDanglingFolderRecord: a folder record pointing at a map_uid
// that storage does not have is dropped (its message is gone).
func TestRebuildDropsDanglingFolderRecord(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "keep me\r\n")

	// Dangling: index references map_uid 999999 which was never stored.
	f, _ := idx.OpenFolder("INBOX", 0)
	uid, _ := idx.AllocateUID(f.ID)
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Filename: "999999", Size: 4}); err != nil {
		t.Fatal(err)
	}
	if got := folderCount(t, idx, "INBOX"); got != 2 {
		t.Fatalf("pre-rebuild INBOX = %d, want 2", got)
	}

	stats, err := box.RebuildStorage(idx)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("post-rebuild INBOX = %d, want 1 (dangling record dropped)", got)
	}
	_ = stats
}

// TestRebuildExpungesVanishedMapRecord: a message whose m.<N> file was deleted
// is dropped from BOTH the folder index and the map.
func TestRebuildExpungesVanishedMapRecord(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "stays in m.1\r\n")
	// A >2 MiB body forces a rotation into m.2, isolating this message.
	big := "X" + strings.Repeat("y", 2*1024*1024+16)
	deliverMsg(t, box, idx, "INBOX", big)

	// Delete m.2 so its message vanishes from storage.
	if err := os.Remove(box.mfilePath(2)); err != nil {
		t.Fatalf("remove m.2: %v", err)
	}

	stats, err := box.RebuildStorage(idx)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.Expunged != 1 {
		t.Errorf("expunged map records = %d, want 1", stats.Expunged)
	}
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX = %d, want 1 (vanished message dropped)", got)
	}
	// Map no longer carries the vanished record.
	m, _ := box.openMap()
	if _, ok, _ := m.Lookup(2); ok {
		t.Error("map still holds the vanished map_uid 2")
	}
}

// TestRebuildBumpsGenerationCounter: the persisted rebuild_count increments per
// rebuild and survives a reopen (header migrated to 8 bytes, stably).
func TestRebuildBumpsGenerationCounter(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "body\r\n")

	s1, err := box.RebuildStorage(idx)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := box.RebuildStorage(idx)
	if err != nil {
		t.Fatal(err)
	}
	if s1.RebuildCount != 1 || s2.RebuildCount != 2 {
		t.Fatalf("rebuild counts = %d,%d, want 1,2", s1.RebuildCount, s2.RebuildCount)
	}

	// Reopen the whole storage and confirm the counter persisted.
	box2, _ := newBoxAndIndex(t, home)
	m, err := box2.openMap()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.RebuildCount(); got != 2 {
		t.Errorf("reopened rebuild_count = %d, want 2", got)
	}
}

// TestRebuildAbortsOnUnmountedAlt: alt storage configured but its directory is
// absent must abort before any expunge (would otherwise mass-delete alt mail).
func TestRebuildAbortsOnUnmountedAlt(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	altHome := filepath.Join(base, "alt")
	box := New(WithAltStorage(filepath.Join(altHome, "%u"))).OpenUser(
		&mailbox.UserInfo{Username: "u@x.io", Home: home}).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "u@x.io", Home: home})
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	// altStoragePath() does not exist → guard must fire.
	_, err := box.RebuildStorage(idx)
	if err == nil || !strings.Contains(err.Error(), "alt storage") {
		t.Fatalf("expected alt-unavailable abort, got %v", err)
	}
}

// TestRebuildAbortsOnIncompleteScan: a corrupt m.<N> makes the scan incomplete,
// and the rebuild must refuse (never expunge on an unreadable tier).
func TestRebuildAbortsOnIncompleteScan(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "good record\r\n")
	// Append garbage with no LF so the next record can never be framed.
	f, err := os.OpenFile(box.mfilePath(1), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte(strings.Repeat("X", 100)))
	_ = f.Close()

	_, err = box.RebuildStorage(idx)
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("expected ErrScanIncomplete abort, got %v", err)
	}
	// Nothing was expunged: the good message still resolves.
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX = %d, want 1 (nothing dropped on abort)", got)
	}
}
