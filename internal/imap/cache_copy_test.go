package imap_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func appendSubject(t *testing.T, rc *rawConn, folder, subject string) {
	t.Helper()
	rc.seq++
	tag := "c" + itoa(rc.seq)
	body := "From: a@example.com\r\nSubject: " + subject + "\r\n\r\nx\r\n"
	rc.conn.Write([]byte(tag + " APPEND " + folder + " {" + itoa(len(body)) + "}\r\n"))
	rc.readLine()
	rc.conn.Write([]byte(body + "\r\n"))
	for !strings.HasPrefix(rc.readLine(), tag+" ") {
	}
}

// A COPY must not carry the cache offset: it is valid only inside its own
// (indexid, file_seq) pair, and the destination folder has another.
//
// The behavioural backstop; the per-guard test is
// TestAppendNeverPersistsACacheOffset. The destination is warmed first so its
// cache holds a valid record at the offsets a travelled one would hit --
// otherwise the wrong answer would be unobservable.
func TestCopy_DoesNotCarryTheCacheOffset(t *testing.T) {
	root, addr := startEnvelopeCacheServer(t)
	c := dialRaw(t, addr)
	c.login()

	c.cmd(`CREATE Archive`)
	// Decoy: a real record at a low offset in Archive's cache.
	appendSubject(t, c, "Archive", "decoy-in-destination")
	c.cmd(`SELECT Archive`)
	if out := c.cmd(`FETCH 1 (ENVELOPE)`); !strings.Contains(out, "decoy-in-destination") {
		t.Fatalf("decoy not cached:\n%s", out)
	}

	// Source, cached: carries a non-zero offset into INBOX's cache.
	appendSubject(t, c, "INBOX", "source-message")
	c.cmd(`SELECT INBOX`)
	if out := c.cmd(`FETCH 1 (ENVELOPE)`); !strings.Contains(out, "source-message") {
		t.Fatalf("source not cached:\n%s", out)
	}

	if out := c.cmd(`COPY 1 Archive`); !strings.Contains(out, "OK") {
		t.Fatalf("copy failed:\n%s", out)
	}

	// The copy's record must carry no offset; a second handle reads what the
	// session wrote.
	home := filepath.Join(root, "test.com", "user")
	ui := file.New().OpenUser(&mailbox.UserInfo{Username: "user@test.com", Home: home})
	f, err := ui.OpenFolder("Archive", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ui.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Archive holds %d messages, want the decoy and the copy", len(msgs))
	}
	copied := msgs[len(msgs)-1]
	if copied.CacheOffset != 0 {
		t.Errorf("the copy carries cache offset %d from the source folder; it is only meaningful "+
			"inside the source's own (indexid, file_seq) pair", copied.CacheOffset)
	}

	// And it answers with its own envelope, not the decoy's.
	c.cmd(`SELECT Archive`)
	out := c.cmd(`FETCH 2 (ENVELOPE)`)
	if !strings.Contains(out, "source-message") {
		t.Errorf("the copy did not answer with its own envelope:\n%s", out)
	}
	if strings.Contains(out, "decoy-in-destination") {
		t.Errorf("the copy answered with the destination's other record:\n%s", out)
	}
}
