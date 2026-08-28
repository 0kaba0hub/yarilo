package mdbox

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Can our own storage rebuild take a store another implementation wrote?
func TestExperimentForeignStoreRebuild(t *testing.T) {
	home := t.TempDir()
	box, idx := newBoxAndIndex(t, home)

	// Their storage directory, our layout: the two agree on
	// mdbox/storage/m.<N>.
	dir := filepath.Join(home, mdboxRoot, storageDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.1"), dboxref.MdboxFile(t), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := box.RebuildStorage(idx, true)
	if err != nil {
		t.Fatalf("rebuild refused: %v", err)
	}
	t.Logf("stats: %+v", stats)

	folders, err := box.ListFolders()
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}
	for _, fe := range folders {
		f, ferr := idx.OpenFolder(fe.Name, 0)
		if ferr != nil {
			t.Fatalf("open %s: %v", fe.Name, ferr)
		}
		msgs, merr := idx.GetMessages(f.ID, mailbox.SeqSet{})
		if merr != nil {
			t.Fatalf("messages in %s: %v", fe.Name, merr)
		}
		t.Logf("folder %q: %d messages", fe.Name, len(msgs))
		for _, m := range msgs {
			rc, fetchErr := box.Fetch(fe.Name, m.Filename, m.AltTier)
			if fetchErr != nil {
				t.Logf("  uid %d: FETCH FAILED: %v", m.UID, fetchErr)
				continue
			}
			body, _ := io.ReadAll(rc)
			_ = rc.Close()
			t.Logf("  uid %d size=%d vsize=%d flags=%v served=%d guid=%x",
				m.UID, m.Size, m.VSize, m.Flags, len(body), m.GUID[:4])
		}
	}
}
