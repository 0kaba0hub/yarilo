package maildir

import (
	"strings"
	"testing"
)

var encodeFlagsTests = []struct {
	flags []string
	want  string
}{
	{nil, ""},
	{[]string{`\Seen`}, "S"},
	{[]string{`\Answered`, `\Seen`}, "RS"},
	{[]string{`\Draft`, `\Flagged`, `\Deleted`, `\Seen`, `\Answered`}, "DFRST"},
	{[]string{`\Deleted`}, "T"},
}

func TestEncodeFlags(t *testing.T) {
	for _, tc := range encodeFlagsTests {
		got := encodeFlags(tc.flags)
		if got != tc.want {
			t.Errorf("encodeFlags(%v) = %q, want %q", tc.flags, got, tc.want)
		}
	}
}

func TestDecodeFlags(t *testing.T) {
	cases := []struct {
		filename string
		flags    []string
	}{
		{"1234567890.M123P456.host:2,S", []string{`\Seen`}},
		{"1234567890.M123P456.host:2,RST", []string{`\Answered`, `\Seen`, `\Deleted`}},
		{"1234567890.M123P456.host:2,", nil},
		{"1234567890.M123P456.host", nil},
		{"1234567890.M123P456.host:2,DFRST", []string{`\Draft`, `\Flagged`, `\Answered`, `\Seen`, `\Deleted`}},
	}
	for _, tc := range cases {
		got, _ := decodeFlags(tc.filename)
		if len(got) != len(tc.flags) {
			t.Errorf("decodeFlags(%q) flags = %v, want %v", tc.filename, got, tc.flags)
			continue
		}
		for i, f := range got {
			if f != tc.flags[i] {
				t.Errorf("decodeFlags(%q)[%d] = %q, want %q", tc.filename, i, f, tc.flags[i])
			}
		}
	}
}

func TestEncodeDecode_Roundtrip(t *testing.T) {
	flags := []string{`\Answered`, `\Flagged`, `\Deleted`, `\Seen`, `\Draft`}
	encoded := encodeFlags(flags)
	filename := "msg:2," + encoded
	decoded, _ := decodeFlags(filename)
	if len(decoded) != len(flags) {
		t.Fatalf("roundtrip: got %v, want %v", decoded, flags)
	}
}

func TestSave_Fetch_Remove(t *testing.T) {
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user := "alice@example.com"
	folder := "INBOX"

	if err := b.Init(user); err != nil {
		t.Fatal(err)
	}

	body := "From: test@example.com\r\nSubject: Test\r\n\r\nHello\r\n"
	filename, err := b.Save(user, folder, strings.NewReader(body), int64(len(body)), []string{`\Seen`})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.Contains(filename, ":2,S") {
		t.Errorf("filename %q should contain ':2,S'", filename)
	}

	rc, err := b.Fetch(user, folder, filename)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	rc.Close()

	if err := b.Remove(user, folder, filename); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Double remove must not error.
	if err := b.Remove(user, folder, filename); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
}

func TestFolderExists(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	ok, err := b.FolderExists("u@x.com", "INBOX")
	if err != nil || !ok {
		t.Fatalf("INBOX should exist after Init, got ok=%v err=%v", ok, err)
	}
	ok, err = b.FolderExists("u@x.com", "NoSuchFolder")
	if err != nil || ok {
		t.Fatalf("NoSuchFolder should not exist, got ok=%v err=%v", ok, err)
	}
}

func TestCreate_Delete(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com") //nolint:errcheck

	if err := b.Create("u@x.com", "Sent"); err != nil {
		t.Fatal(err)
	}
	ok, _ := b.FolderExists("u@x.com", "Sent")
	if !ok {
		t.Fatal("Sent folder should exist after Create")
	}

	if err := b.Delete("u@x.com", "Sent"); err != nil {
		t.Fatal(err)
	}
	ok, _ = b.FolderExists("u@x.com", "Sent")
	if ok {
		t.Fatal("Sent folder should not exist after Delete")
	}
}

func TestListFolders(t *testing.T) {
	b, _ := New(t.TempDir())
	b.Init("u@x.com")             //nolint:errcheck
	b.Create("u@x.com", "Sent")   //nolint:errcheck
	b.Create("u@x.com", "Drafts") //nolint:errcheck

	folders, err := b.ListFolders("u@x.com")
	if err != nil {
		t.Fatal(err)
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
