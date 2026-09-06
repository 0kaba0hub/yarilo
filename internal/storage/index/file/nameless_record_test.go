package file

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A record created with no filename is a defect at the caller: 104 such records
// were found in one store, 10 of them unreachable messages (#1693).
func TestARecordWithNoFilenamePanicsUnderTest(t *testing.T) {
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: "u@example.com", Home: t.TempDir(), SessionID: "s",
	})
	defer ui.Close() //nolint:errcheck
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a record with no filename was accepted: the message it stands for " +
				"cannot be read, and nothing said so")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "no filename") {
			t.Errorf("the panic does not name the defect: %v", r)
		}
	}()
	_ = ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10})
}

// A sidecar that stops mid-scan is an error, not a shorter map: the next flush
// would rewrite it from the partial map, silently (#1693).
func TestATruncatedSidecarIsAnError(t *testing.T) {
	dir := t.TempDir()
	// A line longer than the scanner's token limit: the scan stops on it.
	long := strings.Repeat("x", 128*1024)
	body := "1\tm.1\t10\n2\t" + long + "\t20\n3\tm.3\t30\n"
	if err := os.WriteFile(filepath.Join(dir, IndexNamesFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	names, _, err := loadNames(dir)
	if err == nil {
		t.Fatalf("a truncated scan returned no error and %d names: the next flush would "+
			"rewrite the sidecar from a partial map", len(names))
	}
}

// And a sidecar that reads to the end is not an error.
func TestAWholeSidecarLoads(t *testing.T) {
	dir := t.TempDir()
	body := "1\tm.1\t10\n2\tm.2\t20\n"
	if err := os.WriteFile(filepath.Join(dir, IndexNamesFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	names, sizes, err := loadNames(dir)
	if err != nil {
		t.Fatalf("a whole sidecar failed to load: %v", err)
	}
	if len(names) != 2 || sizes[2] != 20 {
		t.Errorf("loaded %d names, sizes[2]=%d", len(names), sizes[2])
	}
}

// An update with no name keeps the one the record has, and the refusal survives
// a reopen: the sidecar's last line wins on reload (#1693).
func TestAnUpdateWithNoNameKeepsTheName(t *testing.T) {
	dir := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@example.com", Home: dir, SessionID: "s"}
	ui := New().OpenUser(info)
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "m.1", Size: 10}); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("an update with no name was accepted")
			}
		}()
		_ = ui.UpdateFilename(f.ID, 1, "")
	}()
	ui.Close() //nolint:errcheck

	// Reopened from disk: the sidecar must still name the message.
	again := New().OpenUser(info)
	defer again.Close() //nolint:errcheck
	g, err := again.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := again.GetMessages(g.ID, mailbox.SeqSet{{From: 1, To: 1}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("read back %d records: %v", len(msgs), err)
	}
	if msgs[0].Filename != "m.1" {
		t.Errorf("after a refused update the record names %q, want \"m.1\": the update "+
			"erased what it could not replace", msgs[0].Filename)
	}
}

// And the batch path refuses the same way, per uid: the rest of the batch lands.
func TestABatchUpdateSkipsTheNamelessOne(t *testing.T) {
	dir := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: "u@example.com", Home: dir, SessionID: "s"})
	defer ui.Close() //nolint:errcheck
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{1, 2} {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{
			UID: uid, Filename: "m." + string(rune('0'+uid)), Size: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	batch, ok := ui.(mailbox.FilenameWriterMulti)
	if !ok {
		t.Fatal("the index does not take a batch of filenames")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a batch with an empty name was accepted")
			}
		}()
		_ = batch.UpdateFilenames(f.ID, map[uint32]string{1: "", 2: "m.2b"})
	}()
	msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.UID == 1 && m.Filename != "m.1" {
			t.Errorf("uid 1 names %q, want the name it had", m.Filename)
		}
	}
}
