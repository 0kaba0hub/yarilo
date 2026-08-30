package dboxconv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
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
	got, err := dboxconv.ReadForeignSubscriptions(dir, "/")
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
	got, err := dboxconv.ReadForeignSubscriptions(dir, "/")
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
	got, err := dboxconv.ReadForeignSubscriptions(dir, "/")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0] != "one\ttwo" {
		t.Errorf("read as %q, want one name holding a tab", got)
	}
}
