package dbox

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func testHome(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

func newBox(t *testing.T) (*userMailbox, string) {
	t.Helper()
	root := t.TempDir()
	home := testHome(root, "u@x.com")
	return New().OpenUser(&mailbox.UserInfo{Username: "u@x.com", Home: home}).(*userMailbox), root
}

func TestInit(t *testing.T) {
	cases := []struct {
		user string
	}{
		{"alice@example.com"},
		{"bob@mail.local"},
		{"noatuser"},
	}
	for _, tc := range cases {
		root := t.TempDir()
		home := testHome(root, tc.user)
		box := New().OpenUser(&mailbox.UserInfo{Username: tc.user, Home: home}).(*userMailbox)
		if err := box.Init(); err != nil {
			t.Errorf("Init(%q) error: %v", tc.user, err)
		}
		ok, err := box.FolderExists("INBOX")
		if err != nil || !ok {
			t.Errorf("Init(%q): INBOX should exist, ok=%v err=%v", tc.user, ok, err)
		}
	}
}

func TestCreate_Delete(t *testing.T) {
	cases := []struct {
		folder string
	}{
		{"Sent"},
		{"Drafts"},
		{"Archives"},
	}
	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	for _, tc := range cases {
		if err := box.Create(tc.folder); err != nil {
			t.Fatalf("Create(%q): %v", tc.folder, err)
		}
		ok, err := box.FolderExists(tc.folder)
		if err != nil || !ok {
			t.Errorf("Create(%q): folder should exist, ok=%v err=%v", tc.folder, ok, err)
		}
		if err := box.Delete(tc.folder); err != nil {
			t.Fatalf("Delete(%q): %v", tc.folder, err)
		}
		ok, err = box.FolderExists(tc.folder)
		if err != nil || ok {
			t.Errorf("Delete(%q): folder should be gone, ok=%v err=%v", tc.folder, ok, err)
		}
	}
}

func TestSave_Fetch_Roundtrip(t *testing.T) {
	cases := []struct {
		body  string
		flags []string
	}{
		{"From: a@b.com\r\n\r\nHello\r\n", nil},
		{"From: x@y.com\r\nSubject: Hi\r\n\r\nWorld\r\n", []string{`\Seen`}},
		{"", nil},
	}

	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	for _, tc := range cases {
		filename, err := box.Save("INBOX", strings.NewReader(tc.body), int64(len(tc.body)), tc.flags)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if !strings.HasPrefix(filename, "u.") {
			t.Errorf("filename %q must start with 'u.'", filename)
		}

		rc, err := box.Fetch("INBOX", filename)
		if err != nil {
			t.Fatalf("Fetch(%q): %v", filename, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(got) != tc.body {
			t.Errorf("body mismatch: got %q, want %q", got, tc.body)
		}
	}
}

func TestRemove(t *testing.T) {
	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	body := "From: a@b.com\r\n\r\ntest\r\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := box.Remove("INBOX", filename); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Idempotent — must not error on second remove.
	if err := box.Remove("INBOX", filename); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
	// Fetch after remove must error.
	if _, err := box.Fetch("INBOX", filename); err == nil {
		t.Error("Fetch after Remove must return error")
	}
}

func TestList(t *testing.T) {
	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	bodies := []string{
		"From: a@b.com\r\n\r\nMsg1\r\n",
		"From: a@b.com\r\n\r\nMsg2\r\n",
		"From: a@b.com\r\n\r\nMsg3\r\n",
	}
	for _, body := range bodies {
		if _, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != len(bodies) {
		t.Fatalf("List returned %d messages, want %d", len(msgs), len(bodies))
	}
	// All UIDs must be 0 — FileIndex manages UIDs.
	for i, m := range msgs {
		if m.UID != 0 {
			t.Errorf("msgs[%d].UID = %d, want 0", i, m.UID)
		}
		if m.Size == 0 {
			t.Errorf("msgs[%d].Size = 0, want >0", i)
		}
	}
}

func TestList_EmptyFolder(t *testing.T) {
	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatalf("List on empty folder: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestFolderExists(t *testing.T) {
	cases := []struct {
		folder string
		want   bool
	}{
		{"INBOX", true},
		{"NoSuchFolder", false},
		{"Sent", false},
	}

	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	for _, tc := range cases {
		ok, err := box.FolderExists(tc.folder)
		if err != nil {
			t.Errorf("FolderExists(%q) error: %v", tc.folder, err)
		}
		if ok != tc.want {
			t.Errorf("FolderExists(%q) = %v, want %v", tc.folder, ok, tc.want)
		}
	}
}

func TestListFolders(t *testing.T) {
	box, _ := newBox(t)
	box.Init()           //nolint:errcheck
	box.Create("Sent")   //nolint:errcheck
	box.Create("Drafts") //nolint:errcheck

	folders, err := box.ListFolders()
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}

	has := func(name string) bool {
		for _, f := range folders {
			if f == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"INBOX", "Sent", "Drafts"} {
		if !has(want) {
			t.Errorf("ListFolders missing %q, got %v", want, folders)
		}
	}
}

func TestFilenameSequenceMonotonic(t *testing.T) {
	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	var names []string
	for i := 0; i < 5; i++ {
		body := "x"
		fn, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, fn)
	}
	for i := 1; i < len(names); i++ {
		if names[i] <= names[i-1] {
			t.Errorf("filenames not monotonically increasing: %v", names)
		}
	}
}

// TestWireFormat verifies that the dbox file on disk starts with the two magic
// bytes (0x01 0x02), contains the message body, and ends with the post-magic
// sequence followed by metadata lines (G, R, Z, V).
func TestWireFormat(t *testing.T) {
	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	body := "From: a@b.com\r\nSubject: wire\r\n\r\nBody text\r\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	folderDir := filepath.Join(box.home, "INBOX")
	raw, err := os.ReadFile(filepath.Join(folderDir, filename))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	// Magic pre: 0x01 0x02
	if len(raw) < 2 || raw[0] != 0x01 || raw[1] != 0x02 {
		t.Errorf("magic pre: want [01 02], got %s", formatHex(raw[:min(2, len(raw))]))
	}

	// Body must be present somewhere in the file.
	if !bytes.Contains(raw, []byte("Body text")) {
		t.Error("message body not found in dbox file")
	}

	// Post-magic sequence: '\n' 0x01 0x03 '\n'
	postMagic := []byte{'\n', 0x01, 0x03, '\n'}
	if !bytes.Contains(raw, postMagic) {
		t.Error("post-magic [0a 01 03 0a] not found in dbox file")
	}

	// Metadata lines after post-magic.
	tail := string(raw[bytes.Index(raw, postMagic)+len(postMagic):])
	for _, prefix := range []string{"G", "R", "Z", "V"} {
		if !strings.Contains(tail, prefix) {
			t.Errorf("metadata key %q missing from file tail: %q", prefix, tail)
		}
	}
}

// TestConcurrentSave verifies that concurrent saves to the same folder produce
// unique filenames and all messages remain fetchable.
func TestConcurrentSave(t *testing.T) {
	box, _ := newBox(t)
	box.Init() //nolint:errcheck

	const n = 20
	names := make([]string, n)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.Repeat("x", i+1)
			fn, err := box.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
			if err != nil {
				t.Errorf("Save goroutine %d: %v", i, err)
				return
			}
			mu.Lock()
			names[i] = fn
			mu.Unlock()
		}()
	}
	wg.Wait()

	// All names must be non-empty and unique.
	seen := make(map[string]bool, n)
	for i, fn := range names {
		if fn == "" {
			t.Errorf("names[%d] is empty (Save failed)", i)
			continue
		}
		if seen[fn] {
			t.Errorf("duplicate filename %q", fn)
		}
		seen[fn] = true
	}

	// All messages must be fetchable.
	for _, fn := range names {
		if fn == "" {
			continue
		}
		rc, err := box.Fetch("INBOX", fn)
		if err != nil {
			t.Errorf("Fetch(%q): %v", fn, err)
			continue
		}
		rc.Close()
	}
}

func formatHex(b []byte) string {
	out := make([]byte, len(b)*3)
	for i, v := range b {
		const hx = "0123456789abcdef"
		out[i*3] = hx[v>>4]
		out[i*3+1] = hx[v&0xf]
		out[i*3+2] = ' '
	}
	return string(bytes.TrimRight(out, " "))
}
