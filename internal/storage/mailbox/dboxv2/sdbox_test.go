package dboxv2

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func newTestUser(t *testing.T) (*Backend, mailbox.UserMailbox, string) {
	t.Helper()
	home := t.TempDir()
	be := New()
	mb := be.OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	if err := mb.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	return be, mb, home
}

func TestInitCreatesLayout(t *testing.T) {
	_, _, home := newTestUser(t)
	for _, p := range []string{
		filepath.Join(home, "sdbox", "control"),
		filepath.Join(home, "sdbox", "control", "yarilo-uidvalidity"),
		filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestUIDValidityStable(t *testing.T) {
	_, mb, _ := newTestUser(t)
	u := mb.(*userMailbox)
	v1, err := u.UIDValidity()
	if err != nil {
		t.Fatalf("UIDValidity: %v", err)
	}
	v2, err := u.UIDValidity()
	if err != nil {
		t.Fatalf("UIDValidity #2: %v", err)
	}
	if v1 != v2 || v1 == 0 {
		t.Errorf("uidvalidity drift: %d → %d", v1, v2)
	}
}

func TestSaveFetchRoundTrip(t *testing.T) {
	_, mb, home := newTestUser(t)
	body := "From: a@x\r\nTo: b@y\r\nSubject: hi\r\n\r\nbody bytes\r\n"

	name, _, _, err := mb.Save("INBOX", strings.NewReader(body), 7, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasPrefix(name, "u.") {
		t.Errorf("final name = %q, want u.<guid> prefix", name)
	}
	finalPath := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", name)
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("final file not on disk: %v", err)
	}
	// No stray .temp.* must remain after Save returns.
	entries, _ := os.ReadDir(filepath.Dir(finalPath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".temp.") {
			t.Errorf("orphan temp file after Save: %s", e.Name())
		}
	}

	rc, err := mb.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer rc.Close()
	gotBytes, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(gotBytes) != body {
		t.Errorf("fetch body drift:\n got %q\nwant %q", gotBytes, body)
	}
}

func TestSaveCRLFNormalisation(t *testing.T) {
	_, mb, _ := newTestUser(t)
	lf := "line one\nline two\n"
	name, vsize, _, err := mb.Save("INBOX", strings.NewReader(lf), 1, int64(len(lf)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// Save must return the virtual (CRLF) size so the index records it and
	// RFC822.SIZE stays stable: 18 physical bytes + 2 bare LFs = 20 (#892).
	if wantV := uint32(len(lf) + 2); vsize != wantV {
		t.Errorf("Save vsize = %d, want %d", vsize, wantV)
	}
	rc, err := mb.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "line one\r\nline two\r\n"
	if string(got) != want {
		t.Errorf("CRLF body drift:\n got %q\nwant %q", got, want)
	}
}

func TestCopyHardlinks(t *testing.T) {
	_, mb, home := newTestUser(t)
	src, _, _, err := mb.Save("INBOX", strings.NewReader("payload"), 3, 7, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := mb.Create("Sent"); err != nil {
		t.Fatalf("create dest folder: %v", err)
	}
	u := mb.(*userMailbox)
	dst, err := u.Copy("INBOX", src, "Sent", 1)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if dst != "u.1" {
		t.Errorf("copy dst name = %q, want u.1", dst)
	}
	srcInfo, err := os.Stat(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", src))
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstInfo, err := os.Stat(filepath.Join(home, "sdbox", "mailboxes", "Sent", "dbox-Mails", dst))
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Error("Copy did not hardlink — IMAP COPY would not be O(1)")
	}
}

func TestRemoveIdempotent(t *testing.T) {
	_, mb, _ := newTestUser(t)
	if err := mb.Remove("INBOX", "u.9999"); err != nil {
		t.Errorf("Remove of missing file should be no-op, got: %v", err)
	}
}

func TestListAndFolderOps(t *testing.T) {
	_, mb, _ := newTestUser(t)
	for _, uid := range []uint32{1, 2, 3} {
		if _, _, _, err := mb.Save("INBOX", strings.NewReader("msg"), uid, 3, nil, [16]byte{}); err != nil {
			t.Fatalf("save uid=%d: %v", uid, err)
		}
	}
	msgs, err := mb.List("INBOX")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	for _, m := range msgs {
		if !strings.HasPrefix(m.Filename, "u.") {
			t.Errorf("unexpected filename %q, want u.<guid> prefix", m.Filename)
		}
	}

	if err := mb.Create("Sent"); err != nil {
		t.Fatalf("create: %v", err)
	}
	ok, err := mb.FolderExists("Sent")
	if err != nil || !ok {
		t.Errorf("Sent should exist: ok=%v err=%v", ok, err)
	}

	folders, err := mb.ListFolders()
	if err != nil {
		t.Fatalf("listfolders: %v", err)
	}
	got := map[string]bool{}
	for _, f := range folders {
		got[f.Name] = true
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
	if ok, _ := mb.FolderExists("Sent"); ok {
		t.Error("Sent should not exist after rename")
	}

	if err := mb.Delete("Archive"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, _ := mb.FolderExists("Archive"); ok {
		t.Error("Archive should not exist after delete")
	}
}

// TestDeleteRemovesFolderDir locks that Delete removes the whole folder
// directory (mailboxes/<name>), not just its dbox-Mails leaf — otherwise
// an empty mailbox shell is left behind on shared storage.
func TestDeleteRemovesFolderDir(t *testing.T) {
	_, mb, home := newTestUser(t)
	if err := mb.Create("Sent"); err != nil {
		t.Fatalf("create: %v", err)
	}
	folderDir := filepath.Join(home, "sdbox", "mailboxes", "Sent")
	if _, err := os.Stat(folderDir); err != nil {
		t.Fatalf("folder dir not created: %v", err)
	}
	if err := mb.Delete("Sent"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(folderDir); !os.IsNotExist(err) {
		t.Errorf("folder dir shell survived Delete: stat err=%v", err)
	}
}

func TestScanRecoversGUIDAndSize(t *testing.T) {
	_, mb, _ := newTestUser(t)
	body := "hello world"
	name, _, _, err := mb.Save("INBOX", strings.NewReader(body), 42, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasPrefix(name, "u.") {
		t.Errorf("save name = %q, want u.<guid> prefix", name)
	}

	recs, err := mb.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if !strings.HasPrefix(r.Filename, "u.") {
		t.Errorf("filename = %q, want u.<guid> prefix", r.Filename)
	}
	var zero [16]byte
	if r.GUID == zero {
		t.Error("scan did not recover GUID from metadata block")
	}
	if r.VSize == 0 {
		t.Error("scan did not recover virtual size")
	}
	if r.InternalDate.IsZero() {
		t.Error("scan did not recover internal date")
	}
}

func TestListFoldersIgnoresLooseDirs(t *testing.T) {
	_, mb, home := newTestUser(t)
	stray := filepath.Join(home, "sdbox", "mailboxes", "stray")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	folders, err := mb.ListFolders()
	if err != nil {
		t.Fatalf("listfolders: %v", err)
	}
	for _, f := range folders {
		if f.Name == "stray" {
			t.Errorf("stray dir without dbox-Mails should not appear in folders: %v", folders)
		}
	}
}

func TestNeedsCRLFNormalisation(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"already\r\nwrapped\r\n", false},
		{"unix\nstyle\n", true},
		{"mixed\r\nand\nback\r\n", true},
		{"no newlines", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := needsCRLFNormalisation([]byte(tc.in)); got != tc.want {
			t.Errorf("needsCRLFNormalisation(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Save must store a caller-supplied GUID verbatim, and Move must keep it.
func TestSaveGUIDAndMovePreservesIt(t *testing.T) {
	_, mb, _ := newTestUser(t)
	var want [16]byte
	for i := range want {
		want[i] = byte(i + 1)
	}
	name, _, got, err := mb.Save("INBOX", strings.NewReader("body\r\n"), 1, 6, nil, want)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got != want {
		t.Fatalf("Save guid = %x, want %x", got, want)
	}
	metas, err := mb.List("INBOX")
	if err != nil || len(metas) != 1 {
		t.Fatalf("list: %v (%d entries)", err, len(metas))
	}
	if metas[0].GUID != want {
		t.Errorf("List guid = %x, want %x", metas[0].GUID, want)
	}
	if err := mb.Create("Archive"); err != nil {
		t.Fatalf("create: %v", err)
	}
	newName, moved, err := mb.Move("INBOX", "Archive", name, [16]byte{})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved != want {
		t.Errorf("Move guid = %x, want %x", moved, want)
	}
	recs, err := mb.Scan("Archive")
	if err != nil || len(recs) != 1 {
		t.Fatalf("scan: %v (%d records)", err, len(recs))
	}
	if recs[0].Filename != newName || recs[0].GUID != want {
		t.Errorf("scan rec = %q/%x, want %q/%x", recs[0].Filename, recs[0].GUID, newName, want)
	}
	if left, _ := mb.List("INBOX"); len(left) != 0 {
		t.Errorf("source folder still holds %d messages", len(left))
	}
}
