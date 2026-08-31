package maildir_test

import (
	"os"
	"path/filepath"
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
