package file

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestVsizeAggregate covers the hdr-vsize extension: appends fold the per-message
// virtual size into the aggregate, expunge subtracts it, and the aggregate
// survives a re-open (persisted to the base header, self-healed from records).
func TestVsizeAggregate(t *testing.T) {
	root := t.TempDir()
	const user = "alice@test.com"

	idx := openIdx(root, user)
	f, err := idx.OpenFolder("INBOX", 42)
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}

	// Append three messages with distinct virtual sizes.
	msgs := []*mailbox.MessageMeta{
		{UID: 1, VSize: 100, Size: 90},
		{UID: 2, VSize: 200, Size: 180},
		{UID: 3, VSize: 300, Size: 270},
	}
	for _, m := range msgs {
		if err := idx.AppendMessage(f.ID, m); err != nil {
			t.Fatalf("append uid %d: %v", m.UID, err)
		}
	}

	bytes, count, err := idx.FolderVSize(f.ID)
	if err != nil {
		t.Fatalf("FolderVSize: %v", err)
	}
	if bytes != 600 || count != 3 {
		t.Fatalf("after append: bytes=%d count=%d, want 600/3", bytes, count)
	}

	// Expunge the middle message → aggregate drops by its vsize.
	if err := idx.ExpungeMessage(f.ID, 2); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	bytes, count, _ = idx.FolderVSize(f.ID)
	if bytes != 400 || count != 2 {
		t.Fatalf("after expunge: bytes=%d count=%d, want 400/2", bytes, count)
	}

	// Re-open the user (fresh in-memory state): the aggregate must reload from
	// the persisted header + log, self-healed by recalc from records.
	idx2 := openIdx(root, user)
	f2, err := idx2.OpenFolder("INBOX", 42)
	if err != nil {
		t.Fatalf("reopen OpenFolder: %v", err)
	}
	bytes, count, _ = idx2.FolderVSize(f2.ID)
	if bytes != 400 || count != 2 {
		t.Fatalf("after reopen: bytes=%d count=%d, want 400/2", bytes, count)
	}
}

// TestVsizeFallbackToPhysical verifies that a message with no VSize (0) folds
// its physical Size into the aggregate instead of nothing.
func TestVsizeFallbackToPhysical(t *testing.T) {
	root := t.TempDir()
	idx := openIdx(root, "bob@test.com")
	f, err := idx.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 512}); err != nil {
		t.Fatalf("append: %v", err)
	}
	bytes, count, _ := idx.FolderVSize(f.ID)
	if bytes != 512 || count != 1 {
		t.Fatalf("fallback: bytes=%d count=%d, want 512/1", bytes, count)
	}
}
