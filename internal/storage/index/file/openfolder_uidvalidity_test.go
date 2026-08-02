package file

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// seedZeroUIDValidityIndex writes an on-disk index whose header UIDVALIDITY is
// 0, so a subsequent OpenFolder exercises loadModern's repair branch (#658
// follow-up: that repair flushes the index and must run under the distributed
// lock so two openers cannot race competing renders).
func seedZeroUIDValidityIndex(t *testing.T, dir string) {
	t.Helper()
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: testUser, Home: testHome(dir, testUser), Driver: "mdbox",
	}).(*userHandle).ui
	if _, err := ui.OpenFolder("INBOX", 7, ""); err != nil {
		t.Fatalf("seed OpenFolder: %v", err)
	}
	_ = ui.Close()

	indexPath := indexPathFor(ui.indexDir("INBOX"))
	mf, err := mailindex.Open(indexPath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	ri := mf.ToRecreateInput(indexPath)
	ri.Header.UIDValidity = 0
	if _, err := mailindex.Recreate(ri); err != nil {
		t.Fatalf("seed recreate: %v", err)
	}
}

// TestOpenFolderRepairsZeroUIDValidity verifies loadModern's UIDVALIDITY repair
// still works after the #658 follow-up refactor that moved it under the
// distributed lock: a zero UIDVALIDITY on disk is repaired to a non-zero value
// on open, and the repair is persisted (a fresh reader sees the same value).
func TestOpenFolderRepairsZeroUIDValidity(t *testing.T) {
	dir := t.TempDir()
	seedZeroUIDValidityIndex(t, dir)
	newLocker := raceTestLockServer(t)

	open := func() *mailbox.Folder {
		t.Helper()
		ui := New(WithLocker(newLocker())).OpenUser(&mailbox.UserInfo{
			Username: testUser, Home: testHome(dir, testUser), Driver: "mdbox",
		}).(*userHandle).ui
		f, err := ui.OpenFolder("INBOX", 0, "")
		if err != nil {
			t.Fatalf("OpenFolder: %v", err)
		}
		return f
	}

	f := open()
	if f.UIDValidity == 0 {
		t.Fatal("UIDValidity not repaired on open")
	}
	if f2 := open(); f2.UIDValidity != f.UIDValidity {
		t.Fatalf("repaired UIDValidity not persisted: first=%d second=%d", f.UIDValidity, f2.UIDValidity)
	}
}
