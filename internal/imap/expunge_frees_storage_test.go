package imap_test

import (
	"os"
	"path/filepath"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// countMaildirMessages counts message files under every cur/ and new/ directory
// below root — the physical footprint of a maildir mailbox.
func countMaildirMessages(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		parent := filepath.Base(filepath.Dir(path))
		if parent == "cur" || parent == "new" {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}

func appendAndExpunge(t *testing.T, dir string, mb mailbox.MailboxBackend) {
	t.Helper()
	c := startQuotaWarnServer(t, dir, mb, false)
	msg := "From: s@x\r\nTo: user@test.com\r\nSubject: leak\r\n\r\nbody to be deleted\r\n"
	ac := c.Append("INBOX", int64(len(msg)), nil)
	if _, err := ac.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	c.Logout().Wait() //nolint:errcheck
}

// TestExpungeFreesStorageMaildir: after EXPUNGE the message's .eml file must be
// physically unlinked, not just dropped from the index (#633). Before the fix the
// file stayed on disk forever.
func TestExpungeFreesStorageMaildir(t *testing.T) {
	dir := t.TempDir()
	appendAndExpunge(t, dir, maildir.New())

	home := filepath.Join(dir, "test.com", "user")
	if got := countMaildirMessages(t, home); got != 0 {
		t.Errorf("after EXPUNGE the maildir still holds %d message file(s); storage leaked (#633)", got)
	}
}

// TestExpungeFreesStorageMdbox: after EXPUNGE the mdbox map refcount must be
// decremented, so a subsequent purge can reclaim the message (#633). Before the
// fix the refcount was never touched and purge found nothing to reclaim.
func TestExpungeFreesStorageMdbox(t *testing.T) {
	dir := t.TempDir()
	appendAndExpunge(t, dir, mdbox.New())

	home := filepath.Join(dir, "test.com", "user")
	// The auth stub returns IndexDir "~/index", so the mdbox map index lives under
	// <home>/index/storage (the m.<N> payload stays under <home>/mdbox/storage).
	// A fresh handle must resolve to the SAME map or it sees no refcount change.
	u := mdbox.New().OpenUser(&mailbox.UserInfo{
		Username: "user@test.com", Home: home, IndexDir: filepath.Join(home, "index"),
	}).(interface {
		Purge() (mdbox.PurgeStats, error)
	})
	stats, err := u.Purge()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if stats.RecordsExpunged < 1 {
		t.Errorf("purge reclaimed %d records; EXPUNGE never decremented the map refcount (#633): %+v",
			stats.RecordsExpunged, stats)
	}
}
