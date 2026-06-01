package dboxv2

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
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
		filepath.Join(home, "sdbox", "control", "dovecot-uidvalidity"),
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

func TestSaveAssignFetchRoundTrip(t *testing.T) {
	_, mb, home := newTestUser(t)
	body := "From: a@x\r\nTo: b@y\r\nSubject: hi\r\n\r\nbody bytes\r\n"

	temp, err := mb.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasPrefix(temp, ".temp.") {
		t.Errorf("temp name %q should start with .temp.", temp)
	}
	tempPath := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", temp)
	if _, err := os.Stat(tempPath); err != nil {
		t.Fatalf("temp file not on disk: %v", err)
	}

	u := mb.(*userMailbox)
	final, err := u.AssignUID("INBOX", temp, 7)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if final != "u.7" {
		t.Errorf("final name = %q, want u.7", final)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf("temp file still on disk after rename")
	}

	rc, err := mb.Fetch("INBOX", final)
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
	temp, err := mb.Save("INBOX", strings.NewReader(lf), int64(len(lf)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	u := mb.(*userMailbox)
	final, err := u.AssignUID("INBOX", temp, 1)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	rc, err := mb.Fetch("INBOX", final)
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

func TestAssignUIDIdempotent(t *testing.T) {
	_, mb, _ := newTestUser(t)
	temp, err := mb.Save("INBOX", strings.NewReader("x"), 1, nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	u := mb.(*userMailbox)
	if _, err := u.AssignUID("INBOX", temp, 5); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	if _, err := u.AssignUID("INBOX", temp, 5); err != nil {
		t.Errorf("second assign should be no-op: %v", err)
	}
}

func TestCopyHardlinks(t *testing.T) {
	_, mb, home := newTestUser(t)
	temp, err := mb.Save("INBOX", strings.NewReader("payload"), 7, nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	u := mb.(*userMailbox)
	src, err := u.AssignUID("INBOX", temp, 3)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := mb.Create("Sent"); err != nil {
		t.Fatalf("create dest folder: %v", err)
	}
	dstTemp, err := u.Copy("INBOX", src, "Sent")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !strings.HasPrefix(dstTemp, ".temp.") {
		t.Errorf("copied temp name %q should start with .temp.", dstTemp)
	}
	dstFinal, err := u.AssignUID("Sent", dstTemp, 1)
	if err != nil {
		t.Fatalf("assign dst: %v", err)
	}
	srcInfo, err := os.Stat(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", src))
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstInfo, err := os.Stat(filepath.Join(home, "sdbox", "mailboxes", "Sent", "dbox-Mails", dstFinal))
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
	u := mb.(*userMailbox)
	for i, uid := range []uint32{1, 2, 3} {
		t.Logf("save %d", i)
		temp, err := mb.Save("INBOX", strings.NewReader("msg"), 3, nil)
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, err := u.AssignUID("INBOX", temp, uid); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	msgs, err := mb.List("INBOX")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	seen := map[uint32]bool{}
	for _, m := range msgs {
		seen[m.UID] = true
	}
	for _, want := range []uint32{1, 2, 3} {
		if !seen[want] {
			t.Errorf("missing UID %d in list", want)
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

func TestScanRecoversGUIDAndSize(t *testing.T) {
	_, mb, _ := newTestUser(t)
	u := mb.(*userMailbox)
	body := "hello world"
	temp, err := mb.Save("INBOX", strings.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := u.AssignUID("INBOX", temp, 42); err != nil {
		t.Fatalf("assign: %v", err)
	}

	recs, err := mb.Scan("INBOX")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Filename != "u.42" {
		t.Errorf("filename = %q, want u.42", r.Filename)
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

func TestAppendUIDEntryNoOp(t *testing.T) {
	_, mb, _ := newTestUser(t)
	if err := mb.AppendUIDEntry("INBOX", 1, "u.1"); err != nil {
		t.Errorf("AppendUIDEntry should be no-op, got: %v", err)
	}
}

func TestListFoldersIgnoresLooseDirs(t *testing.T) {
	_, mb, home := newTestUser(t)
	// Touch a stray directory that should NOT be reported as a folder
	// (no dbox-Mails subdirectory).
	stray := filepath.Join(home, "sdbox", "mailboxes", "stray")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	folders, err := mb.ListFolders()
	if err != nil {
		t.Fatalf("listfolders: %v", err)
	}
	for _, f := range folders {
		if f == "stray" {
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
