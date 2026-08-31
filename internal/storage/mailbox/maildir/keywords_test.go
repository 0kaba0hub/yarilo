package maildir_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A keyword letter in a filename means what the folder's keyword file says it
// means.
//
// The letters a-z in the ":2," part are indexes into dovecot-keywords, one line
// per keyword as "<index> <name>", the letter being 'a'+index. We used to
// invent a name instead -- kw_a -- so a message the other server had marked
// $Important reached a client as something nothing on disk ever said (#1600).
func TestKeywordLettersAreResolvedThroughTheKeywordFile(t *testing.T) {
	home := t.TempDir()
	mailPath := filepath.Join(home, "Maildir")
	cur := filepath.Join(mailPath, "cur")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	// a -> $Important, b -> $Label1; the message carries both, plus \Seen.
	if err := os.WriteFile(filepath.Join(mailPath, "dovecot-keywords"),
		[]byte("0 $Important\n1 $Label1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "1700000001.M1P1.host,S=20:2,abS"
	if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	box := maildir.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	got := msgs[0].Keywords
	if len(got) != 2 || got[0] != "$Important" || got[1] != "$Label1" {
		t.Errorf("keywords are %v, and the keyword file names them $Important and $Label1", got)
	}
	if !hasFlag(msgs[0].Flags, `\Seen`) {
		t.Errorf("flags are %v, want \\Seen", msgs[0].Flags)
	}
}

// A letter the keyword file does not name stays unread rather than becoming a
// name of our making: nothing on disk says what it means.
func TestAnUnnamedKeywordLetterIsNotInvented(t *testing.T) {
	home := t.TempDir()
	mailPath := filepath.Join(home, "Maildir")
	cur := filepath.Join(mailPath, "cur")
	if err := os.MkdirAll(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mailPath, "dovecot-keywords"), []byte("0 $Important\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 'c' has no line of its own.
	name := "1700000001.M1P1.host,S=20:2,ac"
	if err := os.WriteFile(filepath.Join(cur, name), []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	box := maildir.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	got := msgs[0].Keywords
	if len(got) != 1 || got[0] != "$Important" {
		t.Errorf("keywords are %v, want just $Important -- 'c' is named nowhere", got)
	}
	for _, k := range got {
		if k == "kw_c" {
			t.Error("a keyword name was invented for a letter nothing names")
		}
	}
}

// A flag change reaches the filename, which is where a maildir keeps it.
//
// The store describes itself: rebuild the index from the directory and the
// state comes back. A change that stayed in our index left the store saying
// what each message looked like when it was delivered (#1601).
func TestFlagsAndKeywordsReachTheFilename(t *testing.T) {
	home := t.TempDir()
	box := maildir.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	name, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}

	writer, ok := mailbox.Driver(box).(mailbox.FlagWriter)
	if !ok {
		t.Fatal("the maildir driver does not record flags in storage")
	}
	got, err := writer.WriteFlags("INBOX", name, []string{`\Seen`}, []string{"$Important"})
	if err != nil {
		t.Fatalf("write flags: %v", err)
	}

	// The name carries the system letter and a keyword letter.
	info := got[strings.Index(got, ":2,")+3:]
	if !strings.Contains(info, "S") {
		t.Errorf("the name is %q, and \\Seen was set", got)
	}
	if !strings.ContainsAny(info, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("the name is %q, and a keyword was set", got)
	}
	// And the keyword file names that letter.
	raw, err := os.ReadFile(filepath.Join(home, "Maildir", "dovecot-keywords"))
	if err != nil {
		t.Fatalf("keyword file: %v", err)
	}
	if !strings.Contains(string(raw), "$Important") {
		t.Errorf("the keyword file is %q, and $Important was set", raw)
	}

	// The file is on disk under its new name, and not under the old one.
	if _, err := os.Stat(filepath.Join(home, "Maildir", "cur", got)); err != nil {
		t.Errorf("no file under the new name: %v", err)
	}
	if got != name {
		if _, err := os.Stat(filepath.Join(home, "Maildir", "cur", name)); !os.IsNotExist(err) {
			t.Errorf("the old name is still there: %v", err)
		}
	}

	// What a rebuild from the directory alone would see.
	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if !hasFlag(msgs[0].Flags, `\Seen`) {
		t.Errorf("a rebuild reads flags %v, want \\Seen", msgs[0].Flags)
	}
	if len(msgs[0].Keywords) != 1 || msgs[0].Keywords[0] != "$Important" {
		t.Errorf("a rebuild reads keywords %v, want [$Important]", msgs[0].Keywords)
	}
}

// Two folders allocate their own letters, and neither renumbers the other's.
//
// The letter is an index into the folder's own keyword file, so the same
// keyword can be 'a' in one folder and 'b' in another -- and a letter already
// in use never changes meaning, because every filename already written depends
// on it.
func TestKeywordLettersAreFolderLocalAndNeverRenumbered(t *testing.T) {
	home := t.TempDir()
	box := maildir.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"A", "B"} {
		if err := box.Create(f); err != nil {
			t.Fatal(err)
		}
	}
	writer := mailbox.Driver(box).(mailbox.FlagWriter)

	// Folder A learns $One first, folder B learns $Two first.
	save := func(folder string) string {
		name, _, _, err := box.Save(folder, strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		return name
	}
	a1, b1 := save("A"), save("B")
	if _, err := writer.WriteFlags("A", a1, nil, []string{"$One"}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteFlags("B", b1, nil, []string{"$Two"}); err != nil {
		t.Fatal(err)
	}
	// Now each learns the other's keyword, which must take a new letter rather
	// than move the one already in use.
	a2, b2 := save("A"), save("B")
	if _, err := writer.WriteFlags("A", a2, nil, []string{"$Two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteFlags("B", b2, nil, []string{"$One"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ folder, first, second string }{
		{"A", "$One", "$Two"},
		{"B", "$Two", "$One"},
	} {
		raw, err := os.ReadFile(filepath.Join(home, "Maildir", "."+tc.folder, "dovecot-keywords"))
		if err != nil {
			t.Fatalf("%s keyword file: %v", tc.folder, err)
		}
		want := "0 " + tc.first + "\n1 " + tc.second + "\n"
		if string(raw) != want {
			t.Errorf("%s keyword file is %q, want %q -- the first keyword keeps index 0",
				tc.folder, raw, want)
		}
	}

	// And every message still reads back the keyword it was given.
	for _, tc := range []struct{ folder, kw string }{{"A", "$One"}, {"B", "$Two"}} {
		msgs, err := box.List(tc.folder)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range msgs {
			for _, k := range m.Keywords {
				if k == tc.kw {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s no longer reads back %s", tc.folder, tc.kw)
		}
	}
}

// A new keyword takes the first free letter, not the one after the highest.
//
// Their file can have a hole -- a keyword removed leaves its index unused --
// and the reference fills it (maildir-keywords.c takes the first free slot).
// Counting instead would hand out an index already in use the moment a hole
// exists, and every filename carrying that letter would change meaning.
func TestANewKeywordTakesTheFirstFreeLetter(t *testing.T) {
	home := t.TempDir()
	mailPath := filepath.Join(home, "Maildir")
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(mailPath, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// 0 and 2 taken, 1 free.
	if err := os.WriteFile(filepath.Join(mailPath, "dovecot-keywords"),
		[]byte("0 $One\n2 $Three\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	box := maildir.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	name, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 1, 0, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	writer := mailbox.Driver(box).(mailbox.FlagWriter)
	got, err := writer.WriteFlags("INBOX", name, nil, []string{"$New"})
	if err != nil {
		t.Fatal(err)
	}

	info := got[strings.Index(got, ":2,")+3:]
	if info != "b" {
		t.Errorf("the name carries %q, want \"b\" -- index 1 was free", info)
	}
	raw, err := os.ReadFile(filepath.Join(mailPath, "dovecot-keywords"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "0 $One\n1 $New\n2 $Three\n"
	if string(raw) != want {
		t.Errorf("the keyword file is %q, want %q -- the hole is filled and nothing is renumbered", raw, want)
	}
}
