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

// writeV1Message appends a v1 mdbox single-message record to
// <storage>/m.<file_id> and returns the offset at which it
// landed. Mirrors the format the old yarilo mdbox.Save emitted
// so the legacy reader can be exercised end-to-end without
// keeping the writer in tree.
func writeV1Message(t *testing.T, home string, fileID uint32, body []byte, guid [16]byte, received uint32) uint32 {
	t.Helper()
	storage := StorageDir(home)
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	mpath := filepath.Join(storage, fmt.Sprintf("m.%d", fileID))
	st, _ := os.Stat(mpath)
	off := uint32(0)
	if st != nil {
		off = uint32(st.Size())
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "\x01\x02N %08x %016x\n", 0, len(body))
	buf.Write(body)
	buf.WriteString("\n\x01\x03\n")
	fmt.Fprintf(&buf, "G%x\n", guid)
	fmt.Fprintf(&buf, "R%08x\n", received)
	fmt.Fprintf(&buf, "Z%08x\n", len(body))
	fmt.Fprintf(&buf, "V%08x\n", len(body))
	buf.WriteByte('\n')
	f, err := os.OpenFile(mpath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return off
}

func appendMapLine(t *testing.T, home, folder string, e MapEntry) {
	t.Helper()
	if err := os.MkdirAll(FolderPath(home, folder), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(MapPath(home, folder), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	exp := 0
	if e.Expunged {
		exp = 1
	}
	fmt.Fprintf(f, "%d %d %d %d\n", e.FileID, e.Offset, e.Size, exp)
	f.Close()
}

func randomGUID() [16]byte {
	var g [16]byte
	_, _ = rand.Read(g[:])
	return g
}

func TestListFolders(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"INBOX", ".Sent", ".Drafts", "mdbox-storage"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	folders, err := ListFolders(home)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if len(folders) != 3 || folders[0] != "INBOX" {
		t.Errorf("got %v, want [INBOX Sent Drafts] in some order with INBOX first", folders)
	}
	got := map[string]bool{}
	for _, f := range folders {
		got[f] = true
	}
	for _, w := range []string{"INBOX", "Sent", "Drafts"} {
		if !got[w] {
			t.Errorf("missing %q in %v", w, folders)
		}
	}
	if got["mdbox-storage"] {
		t.Errorf("mdbox-storage leaked into folder list: %v", folders)
	}
}

func TestReadMessageRoundTrip(t *testing.T) {
	home := t.TempDir()
	body := []byte("hello v1 mdbox")
	guid := randomGUID()
	received := uint32(time.Now().Unix())
	off := writeV1Message(t, home, 1, body, guid, received)
	appendMapLine(t, home, "INBOX", MapEntry{FileID: 1, Offset: off, Size: uint32(len(body))})

	entries, err := ReadMap(home, "INBOX")
	if err != nil {
		t.Fatalf("ReadMap: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	m, err := ReadMessage(home, entries[0])
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !bytes.Equal(m.Body, body) {
		t.Errorf("body drift: got %q, want %q", m.Body, body)
	}
	if m.GUID != guid {
		t.Errorf("guid drift: got %x, want %x", m.GUID, guid)
	}
	if m.InternalDate.Unix() != int64(received) {
		t.Errorf("date drift: got %v, want unix %d", m.InternalDate, received)
	}
}

func TestReadMessageMultipleInOneFile(t *testing.T) {
	home := t.TempDir()
	bodies := [][]byte{[]byte("first"), []byte("second body"), []byte("third!")}
	offs := make([]uint32, len(bodies))
	for i, b := range bodies {
		offs[i] = writeV1Message(t, home, 1, b, randomGUID(), 1717000000)
		appendMapLine(t, home, "INBOX", MapEntry{FileID: 1, Offset: offs[i], Size: uint32(len(b))})
	}
	entries, err := ReadMap(home, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	for i, e := range entries {
		m, err := ReadMessage(home, e)
		if err != nil {
			t.Errorf("entry %d: %v", i, err)
			continue
		}
		if !bytes.Equal(m.Body, bodies[i]) {
			t.Errorf("entry %d body drift: got %q, want %q", i, m.Body, bodies[i])
		}
	}
}

func TestReadMapMissingFolder(t *testing.T) {
	home := t.TempDir()
	entries, err := ReadMap(home, "Ghost")
	if err != nil {
		t.Errorf("missing folder should not error: %v", err)
	}
	if entries != nil {
		t.Errorf("missing folder should return nil, got %v", entries)
	}
}

// Note: appendMapLine takes (folder, home) reversed for convenience above;
// the helper call sites use named-field MapEntry so the surface is clear.
