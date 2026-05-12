package dbox

import (
	"io"
	"strings"
	"testing"
)

func TestInit(t *testing.T) {
	cases := []struct {
		user string
	}{
		{"alice@example.com"},
		{"bob@mail.local"},
		{"noatuser"},
	}
	for _, tc := range cases {
		b, err := New(t.TempDir())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := b.Init(tc.user); err != nil {
			t.Errorf("Init(%q) error: %v", tc.user, err)
		}
		ok, err := b.FolderExists(tc.user, "INBOX")
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
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	for _, tc := range cases {
		if err := b.Create("u@x.com", tc.folder); err != nil {
			t.Fatalf("Create(%q): %v", tc.folder, err)
		}
		ok, err := b.FolderExists("u@x.com", tc.folder)
		if err != nil || !ok {
			t.Errorf("Create(%q): folder should exist, ok=%v err=%v", tc.folder, ok, err)
		}
		if err := b.Delete("u@x.com", tc.folder); err != nil {
			t.Fatalf("Delete(%q): %v", tc.folder, err)
		}
		ok, err = b.FolderExists("u@x.com", tc.folder)
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

	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	for _, tc := range cases {
		filename, err := b.Save("u@x.com", "INBOX", strings.NewReader(tc.body), int64(len(tc.body)), tc.flags)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if !strings.HasPrefix(filename, "u.") {
			t.Errorf("filename %q must start with 'u.'", filename)
		}

		rc, err := b.Fetch("u@x.com", "INBOX", filename)
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
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	body := "From: a@b.com\r\n\r\ntest\r\n"
	filename, err := b.Save("u@x.com", "INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := b.Remove("u@x.com", "INBOX", filename); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Idempotent — must not error on second remove.
	if err := b.Remove("u@x.com", "INBOX", filename); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
	// Fetch after remove must error.
	if _, err := b.Fetch("u@x.com", "INBOX", filename); err == nil {
		t.Error("Fetch after Remove must return error")
	}
}

func TestList(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	bodies := []string{
		"From: a@b.com\r\n\r\nMsg1\r\n",
		"From: a@b.com\r\n\r\nMsg2\r\n",
		"From: a@b.com\r\n\r\nMsg3\r\n",
	}
	for _, body := range bodies {
		if _, err := b.Save("u@x.com", "INBOX", strings.NewReader(body), int64(len(body)), nil); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := b.List("u@x.com", "INBOX")
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
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	msgs, err := b.List("u@x.com", "INBOX")
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

	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	for _, tc := range cases {
		ok, err := b.FolderExists("u@x.com", tc.folder)
		if err != nil {
			t.Errorf("FolderExists(%q) error: %v", tc.folder, err)
		}
		if ok != tc.want {
			t.Errorf("FolderExists(%q) = %v, want %v", tc.folder, ok, tc.want)
		}
	}
}

func TestListFolders(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com")             //nolint:errcheck
	b.Create("u@x.com", "Sent")   //nolint:errcheck
	b.Create("u@x.com", "Drafts") //nolint:errcheck

	folders, err := b.ListFolders("u@x.com")
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
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	var names []string
	for i := 0; i < 5; i++ {
		body := "x"
		fn, err := b.Save("u@x.com", "INBOX", strings.NewReader(body), int64(len(body)), nil)
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
