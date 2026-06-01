package mdbox

import (
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

const testUser = "alice@example.com"

func testHome(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

// newBox returns a Backend and a per-user handle for testUser in a fresh TempDir.
func newBox(t *testing.T) (*Backend, *userMailbox) {
	t.Helper()
	root := t.TempDir()
	home := testHome(root, testUser)
	b := New()
	box := b.OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userMailbox)
	return b, box
}

// ---- Init ---------------------------------------------------------------

func TestInit(t *testing.T) {
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ok, err := box.FolderExists("INBOX")
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
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range createDeleteCases {
		t.Run(tc.folder, func(t *testing.T) {
			if err := box.Create(tc.folder); err != nil {
				t.Fatalf("Create: %v", err)
			}
			ok, err := box.FolderExists(tc.folder)
			if err != nil || !ok {
				t.Fatalf("folder should exist after Create, ok=%v err=%v", ok, err)
			}

			if err := box.Delete(tc.folder); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			ok, err = box.FolderExists(tc.folder)
			if err != nil || ok {
				t.Fatalf("folder should not exist after Delete, ok=%v err=%v", ok, err)
			}
		})
	}
}

// ---- Save + Fetch round-trip --------------------------------------------

var saveFetchCases = []struct {
	name  string
	body  string
	flags []string
}{
	{"simple", "From: a@b.com\r\nSubject: Hi\r\n\r\nBody\r\n", nil},
	{"with flags", "From: a@b.com\r\n\r\nTest\r\n", []string{`\Seen`, `\Answered`}},
	{"empty body", "", nil},
}

func TestSave_Fetch_Roundtrip(t *testing.T) {
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range saveFetchCases {
		t.Run(tc.name, func(t *testing.T) {
			r := strings.NewReader(tc.body)
			filename, err := box.Save("INBOX", r, 1, int64(len(tc.body)), tc.flags)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if !strings.Contains(filename, ":") {
				t.Fatalf("filename %q must contain ':'", filename)
			}

			rc, err := box.Fetch("INBOX", filename)
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
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	body := "From: x@y.com\r\n\r\nHello\r\n"
	filename, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := box.Remove("INBOX", filename); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// After remove the message must not appear in List.
	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages after Remove, got %d", len(msgs))
	}

	// Idempotent: removing again must not error.
	if err := box.Remove("INBOX", filename); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
}

// ---- List (with expunged) -----------------------------------------------

func TestList_WithExpunged(t *testing.T) {
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	bodies := []string{
		"From: a\r\n\r\nOne\r\n",
		"From: b\r\n\r\nTwo\r\n",
		"From: c\r\n\r\nThree\r\n",
	}
	filenames := make([]string, len(bodies))
	for i, body := range bodies {
		fn, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
		if err != nil {
			t.Fatalf("Save[%d]: %v", i, err)
		}
		filenames[i] = fn
	}

	// Expunge the middle message.
	if err := box.Remove("INBOX", filenames[1]); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	msgs, err := box.List("INBOX")
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
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range folderExistsCases {
		t.Run(tc.folder, func(t *testing.T) {
			ok, err := box.FolderExists(tc.folder)
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
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("Sent"); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("Drafts"); err != nil {
		t.Fatal(err)
	}

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
	// mdbox-storage must not appear as a folder.
	if has("mdbox-storage") {
		t.Errorf("ListFolders must not expose mdbox-storage")
	}
}

// ---- Rotation -----------------------------------------------------------

func TestRotation(t *testing.T) {
	b, box := newBox(t)
	// Use a tiny threshold so the second save triggers rotation.
	b.rotateThreshold = 10
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	body1 := "From: a\r\n\r\nSmall message\r\n"
	fn1, err := box.Save("INBOX", strings.NewReader(body1), 1, int64(len(body1)), nil)
	if err != nil {
		t.Fatalf("Save1: %v", err)
	}

	body2 := "From: b\r\n\r\nSecond message\r\n"
	fn2, err := box.Save("INBOX", strings.NewReader(body2), 1, int64(len(body2)), nil)
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
		rc, err := box.Fetch("INBOX", tc.fn)
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

// ---- New handle resumes from highest file_id on disk -------------------

func TestNew_ResumesFileID(t *testing.T) {
	root := t.TempDir()
	home := testHome(root, testUser)

	b1 := New()
	b1.rotateThreshold = 10
	box1 := b1.OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userMailbox)
	if err := box1.Init(); err != nil {
		t.Fatal(err)
	}
	body := "From: a\r\n\r\nMsg\r\n"
	// Force two files by saving twice with tiny threshold.
	if _, err := box1.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := box1.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil); err != nil {
		t.Fatal(err)
	}
	highID := box1.currentFileID

	// New handle scanning the same home must discover the same highest file_id.
	b2 := New()
	box2 := b2.OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userMailbox)
	if err := box2.Init(); err != nil {
		t.Fatalf("Init (resume): %v", err)
	}
	if box2.currentFileID != highID {
		t.Errorf("resumed currentFileID = %d, want %d", box2.currentFileID, highID)
	}
}

// TestConcurrentSave verifies that concurrent saves produce unique filenames
// and all messages remain fetchable without data corruption.
func TestConcurrentSave(t *testing.T) {
	_, box := newBox(t)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	const n = 20
	filenames := make([]string, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.Repeat("x", i+1)
			fn, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
			if err != nil {
				t.Errorf("Save goroutine %d: %v", i, err)
				return
			}
			mu.Lock()
			filenames[i] = fn
			bodies[i] = body
			mu.Unlock()
		}()
	}
	wg.Wait()

	// All filenames must be non-empty and unique.
	seen := make(map[string]bool, n)
	for i, fn := range filenames {
		if fn == "" {
			t.Errorf("filenames[%d] empty (Save failed)", i)
			continue
		}
		if seen[fn] {
			t.Errorf("duplicate filename %q", fn)
		}
		seen[fn] = true
	}

	// All messages must be fetchable with correct content.
	for i, fn := range filenames {
		if fn == "" {
			continue
		}
		rc, err := box.Fetch("INBOX", fn)
		if err != nil {
			t.Errorf("Fetch(%q): %v", fn, err)
			continue
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != bodies[i] {
			t.Errorf("Fetch(%q): body mismatch (len got=%d want=%d)", fn, len(got), len(bodies[i]))
		}
	}
}

// TestRotationAndMapIntegrity verifies that after rotation, the map file
// records entries from both the old and new m.* files, and List returns all.
func TestRotationAndMapIntegrity(t *testing.T) {
	b, box := newBox(t)
	b.rotateThreshold = 10
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}

	// Save enough messages to force multiple rotations.
	const n = 5
	var saved []string
	for i := 0; i < n; i++ {
		body := strings.Repeat("y", 20) // exceeds threshold → rotates each time
		fn, err := box.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
		if err != nil {
			t.Fatalf("Save[%d]: %v", i, err)
		}
		saved = append(saved, fn)
	}

	// List must return all n messages (map covers all files).
	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("List after %d saves with rotation: got %d, want %d", n, len(msgs), n)
	}

	// Each saved file must still be fetchable.
	for _, fn := range saved {
		rc, err := box.Fetch("INBOX", fn)
		if err != nil {
			t.Errorf("Fetch(%q) after rotation: %v", fn, err)
			continue
		}
		rc.Close()
	}
}
