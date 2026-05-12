package mdbox

import (
	"io"
	"strings"
	"testing"
)

const testUser = "alice@example.com"

// newBackend returns a Backend with t.TempDir() as root.
func newBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// ---- Init ---------------------------------------------------------------

func TestInit(t *testing.T) {
	b := newBackend(t)
	if err := b.Init(testUser); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ok, err := b.FolderExists(testUser, "INBOX")
	if err != nil || !ok {
		t.Fatalf("INBOX must exist after Init, ok=%v err=%v", ok, err)
	}
}

// ---- Create / Delete ----------------------------------------------------

var createDeleteCases = []struct {
	folder string
}{
	{"Sent"},
	{"Drafts"},
	{"Archive"},
}

func TestCreate_Delete(t *testing.T) {
	b := newBackend(t)
	if err := b.Init(testUser); err != nil {
		t.Fatal(err)
	}

	for _, tc := range createDeleteCases {
		t.Run(tc.folder, func(t *testing.T) {
			if err := b.Create(testUser, tc.folder); err != nil {
				t.Fatalf("Create: %v", err)
			}
			ok, err := b.FolderExists(testUser, tc.folder)
			if err != nil || !ok {
				t.Fatalf("folder should exist after Create, ok=%v err=%v", ok, err)
			}

			if err := b.Delete(testUser, tc.folder); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			ok, err = b.FolderExists(testUser, tc.folder)
			if err != nil || ok {
				t.Fatalf("folder should not exist after Delete, ok=%v err=%v", ok, err)
			}
		})
	}
}

// ---- Save + Fetch round-trip --------------------------------------------

var saveFetchCases = []struct {
	name   string
	body   string
	flags  []string
}{
	{"simple", "From: a@b.com\r\nSubject: Hi\r\n\r\nBody\r\n", nil},
	{"with flags", "From: a@b.com\r\n\r\nTest\r\n", []string{`\Seen`, `\Answered`}},
	{"empty body", "", nil},
}

func TestSave_Fetch_Roundtrip(t *testing.T) {
	b := newBackend(t)
	if err := b.Init(testUser); err != nil {
		t.Fatal(err)
	}

	for _, tc := range saveFetchCases {
		t.Run(tc.name, func(t *testing.T) {
			r := strings.NewReader(tc.body)
			filename, err := b.Save(testUser, "INBOX", r, int64(len(tc.body)), tc.flags)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if !strings.Contains(filename, ":") {
				t.Fatalf("filename %q must contain ':'", filename)
			}

			rc, err := b.Fetch(testUser, "INBOX", filename)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			defer rc.Close()

			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != tc.body {
				t.Errorf("body mismatch: got %q, want %q", got, tc.body)
			}
		})
	}
}

// ---- Remove -------------------------------------------------------------

func TestRemove(t *testing.T) {
	b := newBackend(t)
	if err := b.Init(testUser); err != nil {
		t.Fatal(err)
	}

	body := "From: x@y.com\r\n\r\nHello\r\n"
	filename, err := b.Save(testUser, "INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := b.Remove(testUser, "INBOX", filename); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// After remove the message must not appear in List.
	msgs, err := b.List(testUser, "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages after Remove, got %d", len(msgs))
	}

	// Idempotent: removing again must not error.
	if err := b.Remove(testUser, "INBOX", filename); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
}

// ---- List (with expunged) -----------------------------------------------

func TestList_WithExpunged(t *testing.T) {
	b := newBackend(t)
	if err := b.Init(testUser); err != nil {
		t.Fatal(err)
	}

	bodies := []string{
		"From: a\r\n\r\nOne\r\n",
		"From: b\r\n\r\nTwo\r\n",
		"From: c\r\n\r\nThree\r\n",
	}
	filenames := make([]string, len(bodies))
	for i, body := range bodies {
		fn, err := b.Save(testUser, "INBOX", strings.NewReader(body), int64(len(body)), nil)
		if err != nil {
			t.Fatalf("Save[%d]: %v", i, err)
		}
		filenames[i] = fn
	}

	// Expunge the middle message.
	if err := b.Remove(testUser, "INBOX", filenames[1]); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	msgs, err := b.List(testUser, "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	// Sizes should correspond to bodies[0] and bodies[2].
	wantSizes := []uint32{uint32(len(bodies[0])), uint32(len(bodies[2]))}
	for i, m := range msgs {
		if m.Size != wantSizes[i] {
			t.Errorf("msgs[%d].Size = %d, want %d", i, m.Size, wantSizes[i])
		}
	}
}

// ---- FolderExists -------------------------------------------------------

var folderExistsCases = []struct {
	folder string
	want   bool
}{
	{"INBOX", true},
	{"NoSuch", false},
	{"Sent", false},
}

func TestFolderExists(t *testing.T) {
	b := newBackend(t)
	if err := b.Init(testUser); err != nil {
		t.Fatal(err)
	}

	for _, tc := range folderExistsCases {
		t.Run(tc.folder, func(t *testing.T) {
			ok, err := b.FolderExists(testUser, tc.folder)
			if err != nil {
				t.Fatalf("FolderExists: %v", err)
			}
			if ok != tc.want {
				t.Errorf("FolderExists(%q) = %v, want %v", tc.folder, ok, tc.want)
			}
		})
	}
}

// ---- ListFolders --------------------------------------------------------

func TestListFolders(t *testing.T) {
	b := newBackend(t)
	if err := b.Init(testUser); err != nil {
		t.Fatal(err)
	}
	if err := b.Create(testUser, "Sent"); err != nil {
		t.Fatal(err)
	}
	if err := b.Create(testUser, "Drafts"); err != nil {
		t.Fatal(err)
	}

	folders, err := b.ListFolders(testUser)
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
	// mdbox-storage must not appear as a folder.
	if has("mdbox-storage") {
		t.Errorf("ListFolders must not expose mdbox-storage")
	}
}

// ---- Rotation -----------------------------------------------------------

func TestRotation(t *testing.T) {
	b := newBackend(t)
	// Use a tiny threshold so the second save triggers rotation.
	b.rotateThreshold = 10
	if err := b.Init(testUser); err != nil {
		t.Fatal(err)
	}

	body1 := "From: a\r\n\r\nSmall message\r\n"
	fn1, err := b.Save(testUser, "INBOX", strings.NewReader(body1), int64(len(body1)), nil)
	if err != nil {
		t.Fatalf("Save1: %v", err)
	}

	body2 := "From: b\r\n\r\nSecond message\r\n"
	fn2, err := b.Save(testUser, "INBOX", strings.NewReader(body2), int64(len(body2)), nil)
	if err != nil {
		t.Fatalf("Save2: %v", err)
	}

	// The two messages must be in different m.* files (different file_id prefix).
	fid1 := strings.SplitN(fn1, ":", 2)[0]
	fid2 := strings.SplitN(fn2, ":", 2)[0]
	if fid1 == fid2 {
		t.Errorf("expected rotation: fn1=%q fn2=%q should have different file_ids", fn1, fn2)
	}

	// Both must still be fetchable.
	for _, tc := range []struct {
		fn   string
		body string
	}{
		{fn1, body1},
		{fn2, body2},
	} {
		rc, err := b.Fetch(testUser, "INBOX", tc.fn)
		if err != nil {
			t.Fatalf("Fetch(%q): %v", tc.fn, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != tc.body {
			t.Errorf("body mismatch for %q: got %q want %q", tc.fn, got, tc.body)
		}
	}
}

// ---- New() resumes from highest file_id ---------------------------------

func TestNew_ResumesFileID(t *testing.T) {
	root := t.TempDir()
	b1, _ := New(root)
	b1.rotateThreshold = 10
	if err := b1.Init(testUser); err != nil {
		t.Fatal(err)
	}
	body := "From: a\r\n\r\nMsg\r\n"
	// Force two files by saving twice with tiny threshold.
	if _, err := b1.Save(testUser, "INBOX", strings.NewReader(body), int64(len(body)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b1.Save(testUser, "INBOX", strings.NewReader(body), int64(len(body)), nil); err != nil {
		t.Fatal(err)
	}
	highID := b1.currentFileID

	// New backend scanning the same root must discover the same highest file_id.
	b2, err := New(root)
	if err != nil {
		t.Fatalf("New (resume): %v", err)
	}
	if b2.currentFileID != highID {
		t.Errorf("resumed currentFileID = %d, want %d", b2.currentFileID, highID)
	}
}
