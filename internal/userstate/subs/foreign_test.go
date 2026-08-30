package subs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
)

// Their subscription file, read as a client would see the names.
//
// The oracle is the reference's own listing of subscribed mailboxes over the
// store this fixture came from:
//
//	doveadm mailbox list -s
//	Вхідні
//	Вхідні/Робота
//	Archive
//	Archive/2026
//	INBOX
func TestForeignSubscriptionsAreReadAsNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "subscriptions"), dboxref.Subscriptions(t), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := subs.ReadForeign(filepath.Join(dir, "subscriptions"), "/")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []string{"INBOX", "Archive", "Archive/2026", "Вхідні", "Вхідні/Робота"}
	if len(got) != len(want) {
		t.Fatalf("got %d names %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name %d is %q, want %q", i, got[i], want[i])
		}
	}
}

// A version-1 file: no header, one whole name per line, still modified UTF-7.
func TestAVersionOneSubscriptionFileIsRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "subscriptions"),
		[]byte("INBOX\n&BBIERQRWBDQEPQRW-\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := subs.ReadForeign(filepath.Join(dir, "subscriptions"), "/")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 || got[0] != "INBOX" || got[1] != "Вхідні" {
		t.Errorf("read as %v, want [INBOX Вхідні]", got)
	}
}

// A tab inside a folder name is escaped in their file, and must not be read as
// the level separator: the two are the same byte otherwise.
func TestAnEscapedTabIsNotALevelSeparator(t *testing.T) {
	dir := t.TempDir()
	// "one<TAB>two" as a single level: the tab arrives escaped as 0x01 't'.
	if err := os.WriteFile(filepath.Join(dir, "subscriptions"),
		[]byte("V\t2\n\none\x01ttwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := subs.ReadForeign(filepath.Join(dir, "subscriptions"), "/")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0] != "one\ttwo" {
		t.Errorf("read as %q, want one name holding a tab", got)
	}
}

// Their file, read through the ordinary store, at the path ours would use.
//
// The two share a filename, and on any deployment that does not move the
// control root they share a directory: the control root is the mail path unless
// something says otherwise, and their file lives with the mail. So this is what
// a store nobody has converted yet answers LIST with -- and read as ours it
// answered with the version header as a subscribed folder, tab-joined names,
// and modified UTF-7 that a session then encoded a second time (#1583).
func TestTheStoreReadsTheirFileAtOurPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "subscriptions"), dboxref.Subscriptions(t), 0o600); err != nil {
		t.Fatal(err)
	}
	store := subs.New(dir, "subscriptions", "u1@example.com", "test", nil)
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, want := range []string{"INBOX", "Archive", "Archive/2026", "Вхідні", "Вхідні/Робота"} {
		if _, ok := snap[want]; !ok {
			names := make([]string, 0, len(snap))
			for n := range snap {
				names = append(names, n)
			}
			t.Errorf("subscriptions read as %v, and %q is missing", names, want)
		}
	}
	// The two shapes the old reader produced.
	for _, unwanted := range []string{"V\t2", "Archive\t2026", "&BBIERQRWBDQEPQRW-"} {
		if _, ok := snap[unwanted]; ok {
			t.Errorf("%q is subscribed, and it is a line of their file rather than a folder", unwanted)
		}
	}
	if len(snap) != 5 {
		t.Errorf("%d names subscribed, and their file names five folders", len(snap))
	}
}

// A file of ours is still read as ours: the detection must not claim anything
// that merely has names in it.
func TestOurOwnFileIsUnaffected(t *testing.T) {
	dir := t.TempDir()
	store := subs.New(dir, "subscriptions", "u1@example.com", "test", nil)
	for _, name := range []string{"INBOX", "Вхідні/Робота"} {
		if err := store.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("read back %d names, want 2", len(snap))
	}
	if _, ok := snap["Вхідні/Робота"]; !ok {
		t.Error("a name of ours holding a slash and Cyrillic did not survive a round trip")
	}
}
