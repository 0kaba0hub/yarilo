package dboxv2_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// keysOf returns the metadata keys a dbox file carries, in the order written:
// the letters after the "\n\x01\x03\n" magic, one per line, up to the blank.
func keysOf(t *testing.T, raw []byte) []byte {
	t.Helper()
	at := bytes.Index(raw, []byte("\n\x01\x03\n"))
	if at < 0 {
		t.Fatalf("no metadata block in %d bytes", len(raw))
	}
	var out []byte
	for _, line := range bytes.Split(raw[at+4:], []byte("\n")) {
		if len(line) == 0 {
			break
		}
		out = append(out, line[0])
	}
	return out
}

// What we write has the shape of what the reference wrote: the fixtures are its
// own bytes, so a drift in the header or in the trailer's keys shows up here
// rather than on a store somebody else has to read (#1704).
func TestASavedMessageHasTheReferenceForm(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: home}
	mb := dboxv2.New().OpenUser(info)
	if err := mb.Init(); err != nil {
		t.Fatal(err)
	}
	namer := mailbox.Driver(mb).(mailbox.UIDNamer)
	for uid := uint32(1); uid <= 2; uid++ {
		temp, _, _, err := mb.Save("INBOX", strings.NewReader("From: a@a.com\r\n\r\nbody\r\n"), 0, 0, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := namer.AssignUID("INBOX", temp, uid); err != nil {
			t.Fatal(err)
		}
	}

	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "yarilo.") {
			names = append(names, e.Name())
		}
	}
	if len(names) != 2 || names[0] != "u.1" || names[1] != "u.2" {
		t.Fatalf("the folder lists %v, and the reference's own fixtures are u.1 and u.2", names)
	}

	ours, err := os.ReadFile(filepath.Join(dir, "u.1"))
	if err != nil {
		t.Fatal(err)
	}
	theirs := dboxref.SdboxShort(t)

	if !bytes.HasPrefix(ours, []byte("2 M1e C")) {
		t.Errorf("our file header is %q, theirs is %q", firstLine(ours), firstLine(theirs))
	}
	if got, want := len(firstLine(ours)), len(firstLine(theirs)); got != want {
		t.Errorf("our file header line is %d bytes, theirs %d", got, want)
	}
	ourHdr, theirHdr := messageHeaderOf(t, ours), messageHeaderOf(t, theirs)
	if len(ourHdr) != len(theirHdr) {
		t.Errorf("our message header is %d bytes, theirs %d", len(ourHdr), len(theirHdr))
	}
	if !bytes.Equal(ourHdr[:3], theirHdr[:3]) || ourHdr[len(ourHdr)-1] != theirHdr[len(theirHdr)-1] {
		t.Errorf("our message header is %q, theirs %q", ourHdr, theirHdr)
	}
	if got, want := string(keysOf(t, ours)), string(keysOf(t, theirs)); got != want {
		t.Errorf("our trailer carries keys %q, theirs %q", got, want)
	}
}

func firstLine(b []byte) []byte {
	if at := bytes.IndexByte(b, '\n'); at >= 0 {
		return b[:at+1]
	}
	return b
}

// messageHeaderOf returns the record header that follows the file header line.
func messageHeaderOf(t *testing.T, b []byte) []byte {
	t.Helper()
	start := len(firstLine(b))
	end := bytes.IndexByte(b[start:], '\n')
	if end < 0 {
		t.Fatal("no message header")
	}
	return b[start : start+end+1]
}
