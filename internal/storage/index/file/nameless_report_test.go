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

// A ghost record is reported once per process, and the line names whose mailbox
// it is: 5366 lines for 125 records drowned the slot they were found in, and
// the user had to be recovered from the index path (#1693).
func TestAGhostIsReportedOnceAndNamesItsUser(t *testing.T) {
	home := t.TempDir()
	a := openIdx(home, testUser)
	defer a.Close() //nolint:errcheck
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

	for i := 0; i < 3; i++ {
		if err := a.withFolderLock(fs, func() error { return fs.flush(true) }); err != nil {
			t.Fatal(err)
		}
	}

	got := strings.Count(buf.String(), "named nowhere")
	if got != 1 {
		t.Errorf("one ghost was reported %d times over three flushes: a folder opened all day says it all day", got)
	}
	if !strings.Contains(buf.String(), "user="+testUser) {
		t.Errorf("the line does not name the mailbox it is about: %q", buf.String())
	}
}
