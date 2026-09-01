package folders_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/userstate/folders"
)

func newStore(t *testing.T) (*folders.Store, string) {
	t.Helper()
	dir := t.TempDir()
	return folders.New(dir, "u1@example.com", "owner", nil), dir
}

func TestRecordAndReadBack(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Record("Work", 1788252508, time.Unix(1788252508, 0)); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.UIDValidity("Work")
	if err != nil || !ok || v != 1788252508 {
		t.Fatalf("read back %d, ok=%v, err=%v", v, ok, err)
	}
	if _, ok, _ := s.UIDValidity("Other"); ok {
		t.Error("a folder never recorded came back known")
	}
}

// A delete must forget: RFC 3501 §6.3.4 requires a folder created again under
// the same name to look new, and an entry surviving the delete would hand back
// the old number.
func TestRemoveForgets(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Record("Work", 1788252508, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("Work"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UIDValidity("Work"); ok {
		t.Error("the identity survived the delete")
	}
}

func TestRenameCarriesTheIdentity(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Record("Work", 1788252508, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("Work", "Job"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UIDValidity("Work"); ok {
		t.Error("the old name still has an identity")
	}
	v, ok, _ := s.UIDValidity("Job")
	if !ok || v != 1788252508 {
		t.Errorf("the new name has %d (ok=%v); a rename keeps the number", v, ok)
	}
}

// Folder names carry spaces, and the name is the rest of the line for exactly
// that reason.
func TestANameWithSpacesSurvivesTheRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	const name = "Списки розсилки/Робота і дім"
	if err := s.Record(name, 1788252508, time.Now()); err != nil {
		t.Fatal(err)
	}
	v, ok, _ := s.UIDValidity(name)
	if !ok || v != 1788252508 {
		t.Errorf("%q read back as %d (ok=%v)", name, v, ok)
	}
}

// A line that does not parse must not make a mailbox unopenable: the worst it
// can cost is one folder resynchronising.
func TestAGarbledLineIsSkippedRatherThanFatal(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Record("Work", 1788252508, time.Now()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, folders.FileName)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("+ not-hex nonsense\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.UIDValidity("Work")
	if err != nil {
		t.Fatalf("a garbled line made the record unreadable: %v", err)
	}
	if !ok || v != 1788252508 {
		t.Errorf("read back %d (ok=%v) past the garbled line", v, ok)
	}
}

// Two sessions on one login write this record; neither may lose the other's
// entry.
func TestConcurrentWritersKeepEveryEntry(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := folders.New(dir, "u1@example.com", "owner", nil)
			name := "F" + strings.Repeat("x", i)
			if err := s.Record(name, uint32(1788252508+i), time.Now()); err != nil {
				t.Errorf("record: %v", err)
			}
		}(i)
	}
	wg.Wait()

	s := folders.New(dir, "u1@example.com", "owner", nil)
	for i := 0; i < n; i++ {
		name := "F" + strings.Repeat("x", i)
		v, ok, err := s.UIDValidity(name)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || v != uint32(1788252508+i) {
			t.Errorf("%q came back %d (ok=%v)", name, v, ok)
		}
	}
}
