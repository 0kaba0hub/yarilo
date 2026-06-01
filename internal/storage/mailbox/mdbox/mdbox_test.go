package mdbox

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func newTestUser(t *testing.T) (mailbox.UserMailbox, string) {
	t.Helper()
	home := t.TempDir()
	be := New()
	mb := be.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = mb.Close() })
	return mb, home
}

func TestInitCreatesLayout(t *testing.T) {
	_, home := newTestUser(t)
	for _, p := range []string{
		filepath.Join(home, "mdbox", "storage"),
		filepath.Join(home, "mdbox", "storage", "yarilo.map.index"),
		filepath.Join(home, "mdbox", "mailboxes", "INBOX"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestSaveFetchRoundTrip(t *testing.T) {
	mb, _ := newTestUser(t)
	body := "From: a@x\r\nTo: b@y\r\nSubject: hi\r\n\r\nbody bytes\r\n"
	name, err := mb.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// Filename is a decimal map_uid; first save → "1".
	if name != "1" {
		t.Errorf("first save filename = %q, want \"1\"", name)
	}
	rc, err := mb.Fetch("INBOX", name)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Errorf("body drift:\n got %q\nwant %q", got, body)
	}
}

func TestMultiSaveAssignsDistinctMapUIDs(t *testing.T) {
	mb, _ := newTestUser(t)
	names := []string{}
	for i, body := range []string{"first body", "second", "third one"} {
		name, err := mb.Save("INBOX", strings.NewReader(body), uint32(i+1), int64(len(body)), nil)
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		names = append(names, name)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate filename %q in %v", n, names)
		}
		seen[n] = true
	}
}

func TestSaveCRLFNormalisation(t *testing.T) {
	mb, _ := newTestUser(t)
	lf := "line one\nline two\n"
	name, err := mb.Save("INBOX", strings.NewReader(lf), 1, int64(len(lf)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	rc, err := mb.Fetch("INBOX", name)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	want := "line one\r\nline two\r\n"
	if string(got) != want {
		t.Errorf("CRLF body drift:\n got %q\nwant %q", got, want)
	}
}

func TestCopyIsRefcountOnly(t *testing.T) {
	mb, home := newTestUser(t)
	body := "hello"
	srcName, err := mb.Save("INBOX", strings.NewReader(body), 1, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := mb.Create("Sent"); err != nil {
		t.Fatalf("create: %v", err)
	}

	cop, ok := mb.(interface {
		Copy(srcFolder, srcFilename, dstFolder string, dstUID uint32) (string, error)
	})
	if !ok {
		t.Fatal("mdbox does not implement Copyable interface")
	}
	dstName, err := cop.Copy("INBOX", srcName, "Sent", 1)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if dstName != srcName {
		t.Errorf("dst filename = %q, want same as src %q", dstName, srcName)
	}

	// Source m.<N> should still be a single file; copy must not
	// have written a fresh body.
	entries, _ := os.ReadDir(filepath.Join(home, "mdbox", "storage"))
	mFiles := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "m.") {
			mFiles++
		}
	}
	if mFiles != 1 {
		t.Errorf("Copy created extra m.<N> files: %d, want 1", mFiles)
	}

	// Both source and dest names fetch to the same body.
	for _, n := range []string{srcName, dstName} {
		rc, err := mb.Fetch("Sent", n)
		if err != nil {
			t.Fatalf("fetch %q: %v", n, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != body {
			t.Errorf("fetch %q body drift: %q", n, got)
		}
	}
}

func TestRemoveDecrementsRefcountIdempotent(t *testing.T) {
	mb, _ := newTestUser(t)
	name, err := mb.Save("INBOX", strings.NewReader("payload"), 1, 7, nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := mb.Remove("INBOX", name); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Idempotent: refcount clamps at 0.
	if err := mb.Remove("INBOX", name); err != nil {
		t.Errorf("second remove should be no-op: %v", err)
	}
}

func TestRemoveBadFilename(t *testing.T) {
	mb, _ := newTestUser(t)
	if err := mb.Remove("INBOX", "not-a-number"); err == nil {
		t.Fatal("expected error on bad filename, got nil")
	}
}

func TestFolderOps(t *testing.T) {
	mb, _ := newTestUser(t)
	if err := mb.Create("Sent"); err != nil {
		t.Fatalf("create: %v", err)
	}
	ok, _ := mb.FolderExists("Sent")
	if !ok {
		t.Error("Sent should exist after create")
	}
	folders, _ := mb.ListFolders()
	got := map[string]bool{}
	for _, f := range folders {
		got[f] = true
	}
	if !got["INBOX"] || !got["Sent"] {
		t.Errorf("ListFolders = %v, want INBOX+Sent", folders)
	}
	if err := mb.Rename("Sent", "Archive"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if ok, _ := mb.FolderExists("Archive"); !ok {
		t.Error("Archive should exist after rename")
	}
	if err := mb.Delete("Archive"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, _ := mb.FolderExists("Archive"); ok {
		t.Error("Archive should not exist after delete")
	}
}

func TestScanNotImplemented(t *testing.T) {
	mb, _ := newTestUser(t)
	if _, err := mb.Scan("INBOX"); err == nil {
		t.Fatal("Scan should return not-implemented error")
	}
}
