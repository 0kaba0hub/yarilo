package maildir_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A store another implementation left keeps its UID space when this server
// takes it over.
//
// The uidlist is the same file in both -- version 3, "3 V<uidvalidity>
// N<nextuid>" and then "<uid> :<filename>" -- which is why it is adopted under
// our name rather than converted. What was not adopted was what it says: the
// numbers were parsed past, so the folder got a fresh UIDVALIDITY and fresh
// UIDs, and every client refetched every mailbox (#1593).
func TestAdoptingAMaildirKeepsItsUIDs(t *testing.T) {
	home := t.TempDir()
	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{
		"1700000001.M1P1.host,S=20:2,S",
		"1700000002.M2P2.host,S=20:2,",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Their uidlist, written the way they write it: the name is cut at the info
	// separator, so the record names the base and not the file. That is the
	// point of this fixture -- with full names in it the test would agree with
	// itself and pass over a store no other implementation produces
	// (maildir-uidlist.c cuts at MAILDIR_INFO_SEP before recording).
	uidlist := fmt.Sprintf("3 V1600000000 N42 G0123456789abcdef0123456789abcdef\n40 :%s\n41 :%s\n",
		maildirBaseOf(files[0]), maildirBaseOf(files[1]))
	if err := os.WriteFile(filepath.Join(home, "Maildir", "dovecot-uidlist"), []byte(uidlist), 0o600); err != nil {
		t.Fatal(err)
	}

	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	syncer, ok := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if !ok {
		t.Fatal("the maildir driver no longer reconciles")
	}
	if _, err := syncer.ReconcileIndex(idx, f); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	after, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDValidity != 1600000000 {
		t.Errorf("UIDVALIDITY is %d, and their uidlist says 1600000000", after.UIDValidity)
	}
	if after.NextUID != 42 {
		t.Errorf("next uid is %d, and their uidlist says 42", after.NextUID)
	}

	msgs, err := idx.GetMessages(after.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	for i, want := range []uint32{40, 41} {
		if msgs[i].UID != want {
			t.Errorf("message %d has uid %d, want %d -- their number, not a fresh one", i+1, msgs[i].UID, want)
		}
	}
	// Flags still come from the filenames, which is what made maildir work at
	// all before any of this.
	if !hasFlag(msgs[0].Flags, `\Seen`) {
		t.Errorf("uid 40 has flags %v, and its filename says \\Seen", msgs[0].Flags)
	}
}

// A file the uidlist does not know -- delivered by an MDA after the takeover --
// gets the next UID, as it always did.
func TestAFileTheUIDListDoesNotKnowGetsTheNextUID(t *testing.T) {
	home := t.TempDir()
	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	known := "1700000001.M1P1.host,S=20:2,S"
	fresh := "1700000009.M9P9.host,S=20:2,"
	for _, name := range []string{known, fresh} {
		if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	uidlist := fmt.Sprintf("3 V1600000000 N42 G0123456789abcdef0123456789abcdef\n40 :%s\n", maildirBaseOf(known))
	if err := os.WriteFile(filepath.Join(home, "Maildir", "dovecot-uidlist"), []byte(uidlist), 0o600); err != nil {
		t.Fatal(err)
	}

	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, _ := idx.OpenFolder("INBOX", 0)
	syncer := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if _, err := syncer.ReconcileIndex(idx, f); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after, _ := idx.OpenFolder("INBOX", 0)
	msgs, err := idx.GetMessages(after.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	byName := map[string]uint32{}
	for _, m := range msgs {
		byName[m.Filename] = m.UID
	}
	if byName[known] != 40 {
		t.Errorf("the file their list names has uid %d, want 40", byName[known])
	}
	if byName[fresh] < 42 {
		t.Errorf("the file their list does not name has uid %d; it should come after their next uid, 42", byName[fresh])
	}
}

func hasFlag(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// maildirBaseOf is what the other implementation records: the name up to the
// info separator.
func maildirBaseOf(name string) string {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i]
	}
	return name
}

// Folder names are brought to the encoding this deployment writes.
//
// Their store spells them in modified UTF-7; where that disagrees with
// mailbox_list_utf8, nothing matches by name: the folder is listed as mojibake
// and is not selectable under any name a client can send (#1586, #1593).
//
// The nested row is the one that has to be right level by level. Splitting the
// dotted directory name is safe against an encoded one because modified base64
// is A-Z a-z 0-9 '+' ',' — a '.' cannot appear inside an encoded run, the same
// argument that holds for '/' in the dbox layout.
func TestMaildirFolderNamesAreBroughtToTheConfiguredEncoding(t *testing.T) {
	const (
		encoded      = "&BBIERQRWBDQEPQRW-" // Вхідні
		encodedChild = "&BCAEPgQxBD4EQgQw-" // Робота
	)
	tests := []struct {
		name   string
		utf8   bool
		onDisk []string
		want   []string
	}{
		{
			name:   "utf-8 deployment, their encoding on disk",
			utf8:   true,
			onDisk: []string{"." + encoded, "." + encoded + "." + encodedChild, ".Archive"},
			want:   []string{".Archive", ".Вхідні", ".Вхідні.Робота"},
		},
		{
			name:   "their encoding configured, utf-8 on disk",
			utf8:   false,
			onDisk: []string{".Вхідні", ".Вхідні.Робота", ".Archive"},
			want:   []string{"." + encoded, "." + encoded + "." + encodedChild, ".Archive"},
		},
		{
			// The dot trap, and it is real rather than theoretical: encoding
			// "Звіт.2026" puts the dot *outside* the encoded run, where the
			// layout cannot tell it from a level separator. Splitting on it and
			// converting level by level gives the same bytes as decoding the
			// whole name would, because a dot passes through the encoding
			// untouched -- which is why the two readings agree here and why the
			// ambiguity that remains is the layout's own, not this pass's.
			name:   "a dot the encoding leaves outside its run",
			utf8:   true,
			onDisk: []string{".&BBcEMgRWBEI-.2026"},
			want:   []string{".Звіт.2026"},
		},
		{
			name:   "utf-8 deployment, utf-8 already",
			utf8:   true,
			onDisk: []string{".Вхідні", ".Archive"},
			want:   []string{".Archive", ".Вхідні"},
		},
		{
			name:   "their encoding configured, their encoding already",
			utf8:   false,
			onDisk: []string{"." + encoded, ".Archive"},
			want:   []string{"." + encoded, ".Archive"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			mailPath := filepath.Join(home, "Maildir")
			for _, d := range tc.onDisk {
				if err := os.MkdirAll(filepath.Join(mailPath, d, "cur"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			box := maildir.New(maildir.WithListUTF8(tc.utf8)).
				OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
			defer box.Close() //nolint:errcheck
			if err := box.Init(); err != nil {
				t.Fatalf("init: %v", err)
			}

			got := dottedDirs(t, mailPath)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("directories are %v, want %v", got, tc.want)
			}
			for _, d := range tc.want {
				if _, err := os.Stat(filepath.Join(mailPath, d, "cur")); err != nil {
					t.Errorf("%s lost its cur/: %v", d, err)
				}
			}
		})
	}
}

// A level that cannot be read as theirs is left exactly as it is: it may be
// what a user typed.
func TestAMaildirNameThatDoesNotDecodeIsLeftAlone(t *testing.T) {
	home := t.TempDir()
	mailPath := filepath.Join(home, "Maildir")
	const odd = ".&notbase64$$"
	if err := os.MkdirAll(filepath.Join(mailPath, odd, "cur"), 0o700); err != nil {
		t.Fatal(err)
	}
	box := maildir.New(maildir.WithListUTF8(true)).
		OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := dottedDirs(t, mailPath); len(got) != 1 || got[0] != odd {
		t.Errorf("directories are %v, want %q untouched", got, odd)
	}
}

// Two folders must never become one.
func TestMaildirNameAdoptionRefusesToMerge(t *testing.T) {
	home := t.TempDir()
	mailPath := filepath.Join(home, "Maildir")
	for _, d := range []string{".Вхідні", ".&BBIERQRWBDQEPQRW-"} {
		if err := os.MkdirAll(filepath.Join(mailPath, d, "cur"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	box := maildir.New(maildir.WithListUTF8(true)).
		OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err == nil {
		t.Error("two folders were merged into one name")
	}
	if got := dottedDirs(t, mailPath); len(got) != 2 {
		t.Errorf("directories are %v; nothing should have been lost", got)
	}
}

func dottedDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
