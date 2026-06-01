package v1legacy

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeV1File synthesises one pre-Phase-3 dbox file at path with
// the layout the old internal/storage/mailbox/dbox driver used to
// emit. Mirrors that driver's Save() exactly so the legacy reader
// can be exercised end-to-end without keeping the old code in
// tree.
func writeV1File(t *testing.T, dir, name string, body []byte, guid [16]byte, received uint32) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteByte(0x01)
	buf.WriteByte(0x02)
	fmt.Fprintf(&buf, " N %08x %016x\n", 0, len(body))
	buf.Write(body)
	buf.WriteString("\n\x01\x03\n")
	fmt.Fprintf(&buf, "G%x\n", guid)
	fmt.Fprintf(&buf, "R%08x\n", received)
	fmt.Fprintf(&buf, "Z%08x\n", len(body))
	fmt.Fprintf(&buf, "V%08x\n", len(body))
	buf.WriteByte('\n')
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func randomGUID() [16]byte {
	var g [16]byte
	_, _ = rand.Read(g[:])
	return g
}

func TestListFolders(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "INBOX"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".Sent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".Drafts"), 0o700); err != nil {
		t.Fatal(err)
	}
	folders, err := ListFolders(home)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(folders) != 3 || folders[0] != "INBOX" {
		t.Errorf("got %v, want [INBOX, Sent, Drafts] in some order with INBOX first", folders)
	}
	got := map[string]bool{}
	for _, f := range folders {
		got[f] = true
	}
	for _, want := range []string{"INBOX", "Sent", "Drafts"} {
		if !got[want] {
			t.Errorf("missing folder %s in %v", want, folders)
		}
	}
}

func TestReadMessageRoundTrip(t *testing.T) {
	home := t.TempDir()
	body := []byte("hello world")
	guid := randomGUID()
	received := uint32(time.Now().Unix())
	writeV1File(t, filepath.Join(home, "INBOX"), "u.0000000000000001", body, guid, received)

	msgs, err := ListMessages(home, "INBOX")
	if err != nil {
		t.Fatalf("list msgs: %v", err)
	}
	if len(msgs) != 1 || msgs[0] != "u.0000000000000001" {
		t.Fatalf("got %v, want [u.0000000000000001]", msgs)
	}
	m, err := ReadMessage(home, "INBOX", msgs[0])
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(m.Body, body) {
		t.Errorf("body drift: got %q, want %q", m.Body, body)
	}
	if m.Size != uint32(len(body)) {
		t.Errorf("size = %d, want %d", m.Size, len(body))
	}
	if m.VSize != uint32(len(body)) {
		t.Errorf("vsize = %d, want %d", m.VSize, len(body))
	}
	if m.GUID != guid {
		t.Errorf("guid drift: got %x, want %x", m.GUID, guid)
	}
	if m.InternalDate.Unix() != int64(received) {
		t.Errorf("date = %v, want unix %d", m.InternalDate, received)
	}
}

func TestReadMessageMissingFolder(t *testing.T) {
	home := t.TempDir()
	msgs, err := ListMessages(home, "Ghost")
	if err != nil {
		t.Errorf("missing folder should not error: %v", err)
	}
	if msgs != nil {
		t.Errorf("missing folder should return nil, got %v", msgs)
	}
}

func TestReadMessageBadMagic(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "INBOX"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "INBOX", "u.0000000000000001"), []byte("not a dbox file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMessage(home, "INBOX", "u.0000000000000001"); err == nil {
		t.Fatal("expected error on bad magic")
	}
}

func TestFolderPath(t *testing.T) {
	cases := []struct {
		folder string
		want   string
	}{
		{"INBOX", "/h/INBOX"},
		{"Sent", "/h/.Sent"},
		{"Foo.Bar", "/h/.Foo.Bar"},
	}
	for _, tc := range cases {
		if got := FolderPath("/h", tc.folder); got != tc.want {
			t.Errorf("FolderPath(%q) = %q, want %q", tc.folder, got, tc.want)
		}
	}
}
