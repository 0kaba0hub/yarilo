package maildir_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// uidlistHeader returns the V and N fields of a folder's list.
func uidlistHeader(t *testing.T, path string) (v, n uint32) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uidlist: %v", err)
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	for _, fld := range strings.Fields(line)[1:] {
		num, cerr := strconv.ParseUint(fld[1:], 10, 32)
		if cerr != nil {
			continue
		}
		switch fld[0] {
		case 'V':
			v = uint32(num)
		case 'N':
			n = uint32(num)
		}
	}
	return v, n
}

func uidlistRecords(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uidlist: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		return nil
	}
	return lines[1:]
}

// The list is the UIDVALIDITY of a maildir folder and the index takes it: two
// numbers minted separately are two answers to one question (#1701).
func TestTheIndexTakesItsUIDValidityFromTheList(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, home string)
	}{
		{"a folder that has just been created", func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, home string) {
			if _, err := idx.OpenFolder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"a folder opened before anything was delivered", func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, home string) {
			f, err := idx.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			reconcile(t, box, idx, f)
		}},
		{"a folder whose first messages arrived with no uid", func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, home string) {
			// Delivery that does not know a uid writes no record, so the list is
			// born later, when the folder already holds mail.
			if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			f, err := idx.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			reconcile(t, box, idx, f)
		}},
		{"a folder delivered into first", func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, home string) {
			if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := idx.OpenFolder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
			box := maildir.New().OpenUser(info)
			defer box.Close() //nolint:errcheck
			if err := box.Create("INBOX"); err != nil {
				t.Fatal(err)
			}
			idx := indexfile.New().OpenUser(info)
			defer idx.Close() //nolint:errcheck

			tc.prepare(t, box, idx, home)

			f, err := idx.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), f.NextUID, 0, nil, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			reconcile(t, box, idx, f)

			v, _ := uidlistHeader(t, filepath.Join(home, "Maildir", maildir.UIDListFileName))
			after, err := idx.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			if after.UIDValidity != v {
				t.Errorf("the index says uidvalidity %d, the list says %d: two numbers for one folder",
					after.UIDValidity, v)
			}
		})
	}
}

func reconcile(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) {
	t.Helper()
	syncer, ok := box.(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if !ok {
		t.Fatal("the maildir driver no longer reconciles")
	}
	if _, err := syncer.ReconcileIndex(idx, f); err != nil {
		t.Fatal(err)
	}
}

// The header's next uid is maintained, or a reader trusting it hands out a
// number already in use (#1701).
func TestTheHeaderNextUIDFollowsTheRecords(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}

	for uid := uint32(1); uid <= 3; uid++ {
		if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), uid, 0, nil, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	_, n := uidlistHeader(t, filepath.Join(home, "Maildir", maildir.UIDListFileName))
	if n != 4 {
		t.Errorf("the header says next uid %d after uids 1..3, want 4", n)
	}
}

// A name that carries its sizes needs no keys; a name that does not carries
// them measured from the file, never copied from elsewhere (#1701).
func TestSizeKeysOnlyWhereTheNameLacksThem(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(home, "Maildir", maildir.UIDListFileName)
	for _, rec := range uidlistRecords(t, list) {
		if strings.Contains(rec, " S") || strings.Contains(rec, " W") {
			t.Errorf("a delivered name carries its sizes, and the record repeats them: %q", rec)
		}
	}

	// A file whose name carries no sizes: an adopted store, or a hand-dropped
	// message. Body chosen so the two numbers differ: one lone LF.
	body := "From: a@b\r\n\r\nx\n"
	foreign := "1700000001.M1P1.host:2,S"
	if err := os.WriteFile(filepath.Join(home, "Maildir", "cur", foreign), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 2, 0, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	syncer := box.(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if _, err := syncer.ReconcileIndex(idx, f); err != nil {
		t.Fatal(err)
	}

	var got string
	for _, rec := range uidlistRecords(t, list) {
		if strings.HasSuffix(rec, ":1700000001.M1P1.host") {
			got = rec
		}
	}
	if got == "" {
		t.Fatalf("the foreign name is not in the list: %v", uidlistRecords(t, list))
	}
	wantS := "S" + strconv.Itoa(len(body))
	wantW := "W" + strconv.Itoa(len(body)+1) // one lone LF gains a CR
	if !strings.Contains(got, wantS) || !strings.Contains(got, wantW) {
		t.Errorf("the record is %q, want the sizes the file measures: %s and %s", got, wantS, wantW)
	}
}

// The list is replaced whole, under the lock a foreign writer looks for, and
// nothing of the write is left behind (#1701).
func TestTheListIsReplacedWholeAndLeavesNothingBehind(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 3; uid++ {
		if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), uid, 0, nil, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(home, "Maildir"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), maildir.UIDListFileName) && e.Name() != maildir.UIDListFileName {
			t.Errorf("the write left %q beside the list", e.Name())
		}
	}
}

// A line no rule explains ends the list: what follows is unreachable, and the
// next write must not carry the wreck forward in silence (#1701).
func TestATornListIsRewrittenAndSaidOutLoud(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(home, "Maildir", maildir.UIDListFileName)
	raw, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	// A torn tail: half a record, the way an interrupted append leaves one.
	if err := os.WriteFile(list, append(raw, []byte("2 W16 S1")...), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 2, 0, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no rule explains") {
		t.Errorf("a torn list was carried forward in silence: %q", buf.String())
	}
	for _, rec := range uidlistRecords(t, list) {
		if _, _, ok := strings.Cut(rec, " :"); !ok {
			t.Errorf("the rewritten list still holds a line nothing can read: %q", rec)
		}
	}
}

// The dotlock is what a foreign writer watches for: ours is the lock service,
// and a process that does not speak to it sees only this file (#1701).
func TestAHeldDotlockStopsTheWrite(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(home, "Maildir", maildir.UIDListFileName+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lock) //nolint:errcheck

	_, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, [16]byte{})
	if err == nil {
		t.Fatal("the list was written while another writer held the lock")
	}
	if !strings.Contains(err.Error(), "held by another process") {
		t.Errorf("the error does not name the holder: %v", err)
	}
}

// An adopted message keeps no sizes in its name, so the record must carry them
// wherever it is written -- a move records the message afresh (#1701).
func TestAMovedForeignNameCarriesMeasuredSizes(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	for _, folder := range []string{"INBOX", "Archive"} {
		if err := box.Create(folder); err != nil {
			t.Fatal(err)
		}
	}
	body := "From: a@b\r\n\r\nx\n" // one lone LF: the two numbers differ
	foreign := "1700000001.M1P1.host:2,S"
	if err := os.WriteFile(filepath.Join(home, "Maildir", "cur", foreign), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// A name already taken in the target forces the fresh-name path, which is
	// where the record is written.
	if err := os.WriteFile(filepath.Join(home, "Maildir", ".Archive", "cur", foreign), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := box.Move("INBOX", "Archive", foreign, [16]byte{}); err != nil {
		t.Fatal(err)
	}

	list := filepath.Join(home, "Maildir", ".Archive", maildir.UIDListFileName)
	recs := uidlistRecords(t, list)
	if len(recs) != 1 {
		t.Fatalf("the move recorded %d entries, want 1: %v", len(recs), recs)
	}
	wantS := "S" + strconv.Itoa(len(body))
	wantW := "W" + strconv.Itoa(len(body)+1)
	if !strings.Contains(recs[0], wantS) || !strings.Contains(recs[0], wantW) {
		t.Errorf("the record is %q, want the sizes the file measures: %s and %s", recs[0], wantS, wantW)
	}
}

// The list reaches the disk before it takes its name: a crash after the rename
// and before the data would leave a list of zero length (#1701).
func TestTheListIsSyncedBeforeItIsRenamed(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}

	var order []string
	var atRename string
	maildir.SetTestWriteSeams(
		func(f *os.File) error { order = append(order, "sync"); return f.Sync() },
		func(tmp string) {
			order = append(order, "rename")
			raw, err := os.ReadFile(tmp)
			if err != nil {
				t.Errorf("read the temp list: %v", err)
			}
			atRename = string(raw)
		})
	defer maildir.SetTestWriteSeams(nil, nil)

	if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 7, 0, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[len(order)-2] != "sync" || order[len(order)-1] != "rename" {
		t.Fatalf("the write went %v, want the sync before the rename", order)
	}
	if !strings.Contains(atRename, "\n7 :") || !strings.HasPrefix(atRename, "3 V") {
		t.Errorf("the file renamed into place is not the whole list: %q", atRename)
	}
}
