package file

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// openUserAt opens a second view of the same store, the way a second session
// does: its own folderState over the same directory.
func openUserAt(t *testing.T, root, session string) *userIndex {
	t.Helper()
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: testUser, Home: testHome(root, testUser), SessionID: session,
	}).(*userHandle).ui
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck
	return ui
}

// folderStateFor reaches the open state for a folder, so a test can drive the
// wholesale flush the way stampLineage does.
func (u *userIndex) folderStateFor(t *testing.T, folder string) *folderState {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, fs := range u.open {
		if fs.folder == folder {
			return fs
		}
	}
	t.Fatalf("folder %q not open", folder)
	return nil
}

// A record is recoverable from the log, a name is not: the sidecar is rewritten
// wholesale from one state's map, so a name another state has just appended is
// gone with no writer at fault (#1693).
func TestAWholesaleRewriteKeepsANameAnotherStateJustWrote(t *testing.T) {
	home := t.TempDir()
	a := openUserAt(t, home, "a")
	fa, err := a.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: 1, Filename: "m.1", Size: 10}); err != nil {
		t.Fatal(err)
	}

	// The second state reads the folder as it is now, and does not look again.
	b := openUserAt(t, home, "b")
	if _, err := b.OpenFolder("INBOX", 0, ""); err != nil {
		t.Fatal(err)
	}

	if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: 2, Filename: "m.2", Size: 20}); err != nil {
		t.Fatal(err)
	}

	// A wholesale flush with no reload before it: what stampLineage does.
	fsb := b.folderStateFor(t, "INBOX")
	if err := fsb.flush(true); err != nil {
		t.Fatal(err)
	}

	c := openUserAt(t, home, "c")
	fc, err := c.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := c.GetMessages(fc.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint32]string{}
	for _, m := range msgs {
		got[m.UID] = m.Filename
	}
	if got[2] != "m.2" {
		t.Errorf("uid 2 names %q, want \"m.2\": the wholesale rewrite dropped a name it did not have in memory", got[2])
	}
	if got[1] != "m.1" {
		t.Errorf("uid 1 names %q, want \"m.1\"", got[1])
	}
}

// The index's records are the authority: a uid it no longer carries is not
// carried into the new sidecar either, or an expunge would be undone.
func TestAWholesaleRewriteDropsTheNameOfAnExpungedRecord(t *testing.T) {
	home := t.TempDir()
	a := openUserAt(t, home, "a")
	f, err := a.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 2; uid++ {
		m := &mailbox.MessageMeta{UID: uid, Filename: "m." + string(rune('0'+uid)), Size: 10}
		if err := a.AppendMessage(f.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ExpungeMessage(f.ID, 1); err != nil {
		t.Fatal(err)
	}
	fs := a.folderStateFor(t, "INBOX")
	if err := fs.flush(true); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(fs.indexDir, IndexNamesFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(line, "1\t") {
			t.Errorf("the sidecar still names expunged uid 1: %q", line)
		}
	}
}

// After the merge this is the only way left to hold a record no reader can
// resolve, so it is the one that must speak. It is not a caller's defect -- a
// migrated index arrives before anything has named its records -- so it is said
// in a line, not a panic.
func TestALiveRecordWithNoNameAnywhereIsReported(t *testing.T) {
	home := t.TempDir()
	a := openUserAt(t, home, "a")
	f, err := a.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "m.1", Size: 10}); err != nil {
		t.Fatal(err)
	}
	fs := a.folderStateFor(t, "INBOX")
	if fs.namesFD != nil {
		fs.namesFD.Close() //nolint:errcheck
		fs.namesFD = nil
	}
	if err := os.Remove(filepath.Join(fs.indexDir, IndexNamesFileName)); err != nil {
		t.Fatal(err)
	}
	delete(fs.filenames, 1)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	if err := fs.flush(true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "named nowhere") {
		t.Fatalf("a live record with no name anywhere was written out in silence: %q", buf.String())
	}
}
