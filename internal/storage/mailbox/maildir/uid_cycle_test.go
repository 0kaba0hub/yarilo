package maildir_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func listOf(t *testing.T, path string) (header string, records []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return lines[0], lines[1:]
}

func headerNextUID(t *testing.T, header string) uint32 {
	t.Helper()
	for _, f := range strings.Fields(header)[1:] {
		if f[0] == 'N' {
			n, err := strconv.ParseUint(f[1:], 10, 32)
			if err != nil {
				t.Fatalf("header %q: %v", header, err)
			}
			return uint32(n)
		}
	}
	t.Fatalf("no next uid in %q", header)
	return 0
}

// The list is written where the uid is handed out: waiting for a reconcile left
// it naming neither the message nor the right next uid (#1703, #1700).
func TestTheListCarriesAnAppendBeforeAnyReconcile(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}

	// What an APPEND does, and nothing else: no reconcile anywhere near it.
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Filename: saved, Size: 16, VSize: vsize, GUID: guid}
	if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", m); err != nil {
		t.Fatal(err)
	}

	header, records := listOf(t, filepath.Join(home, "Maildir", maildir.UIDListFileName))
	if len(records) != 1 {
		t.Fatalf("the list holds %v, want one record for the message just appended", records)
	}
	if !strings.HasPrefix(records[0], strconv.FormatUint(uint64(m.UID), 10)+" ") {
		t.Errorf("the record is %q, and the message has uid %d", records[0], m.UID)
	}
	after, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := headerNextUID(t, header); got != after.NextUID {
		t.Errorf("the list says next uid %d, the index says %d", got, after.NextUID)
	}
}

// A move into a folder where the name is taken took the collision path, which
// is where the uid-less record was written (#1703).
func TestAMoveIntoATakenNameIsRecordedUnderTheDestinationUID(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	for _, folder := range []string{"INBOX", "Archive"} {
		if err := box.Create(folder); err != nil {
			t.Fatal(err)
		}
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	archive, err := idx.OpenFolder("Archive", 0)
	if err != nil {
		t.Fatal(err)
	}

	body := "From: a@b\r\n\r\nx\r\n"
	src, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, 0, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	// The same base name already in the destination: the collision path.
	if err := os.WriteFile(filepath.Join(home, "Maildir", ".Archive", "cur", src), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, guid, err := box.Move("INBOX", "Archive", src, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Filename: moved, Size: 16, VSize: 16, GUID: guid}
	if err := mailbox.RecordSaved(idx, box, archive.ID, "Archive", m); err != nil {
		t.Fatal(err)
	}

	_, records := listOf(t, filepath.Join(home, "Maildir", ".Archive", maildir.UIDListFileName))
	if len(records) != 1 {
		t.Fatalf("the list holds %v, want the moved message", records)
	}
	if !strings.HasPrefix(records[0], strconv.FormatUint(uint64(m.UID), 10)+" ") {
		t.Errorf("the record is %q, and the destination gave uid %d", records[0], m.UID)
	}
	for _, rec := range records {
		if strings.HasPrefix(rec, "0 ") {
			t.Errorf("a record with no uid survived: %q", rec)
		}
	}
}

// And the writer refuses one, so the shape holds where no caller is in sight.
func TestTheWriterRefusesARecordWithNoUID(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	saved, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	namer, ok := box.(mailbox.UIDNamer)
	if !ok {
		t.Fatal("the maildir driver does not record a message against its uid")
	}
	if _, err := namer.AssignUID("INBOX", saved, 0); err == nil {
		t.Fatal("a record with no uid was accepted: it maps nothing")
	}
}
