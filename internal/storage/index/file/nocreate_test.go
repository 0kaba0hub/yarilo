package file

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A missing index must be reported, never invented: a fresh one reads as a
// healthy empty folder and hides whatever the store actually held.
func TestNoCreateRefusesMissingIndex(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.test", Home: home}

	idx := New(WithNoCreate()).OpenUser(info)
	t.Cleanup(func() { _ = idx.Close() })
	if _, err := idx.OpenFolder("INBOX", 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenFolder err = %v, want os.ErrNotExist", err)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	for _, e := range entries {
		if e.Name() == IndexFileName {
			t.Errorf("a refused open still wrote %s", filepath.Join(home, e.Name()))
		}
	}
}

// An index that exists opens normally with the option set: it only blocks
// creation, never reading.
func TestNoCreateOpensExistingIndex(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.test", Home: home}

	seed := New().OpenUser(info)
	folder, err := seed.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	wantUIDValidity := folder.UIDValidity
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	idx := New(WithNoCreate()).OpenUser(info)
	t.Cleanup(func() { _ = idx.Close() })
	got, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("OpenFolder on an existing index: %v", err)
	}
	if got.UIDValidity != wantUIDValidity {
		t.Errorf("UIDVALIDITY = %d, want %d (the folder was reinitialised)", got.UIDValidity, wantUIDValidity)
	}
}

// Without the option the old behaviour stands: a first open establishes the
// folder, which is what the services rely on.
func TestDefaultStillCreates(t *testing.T) {
	home := t.TempDir()
	idx := New().OpenUser(&mailbox.UserInfo{Username: "u@x.test", Home: home})
	t.Cleanup(func() { _ = idx.Close() })
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
}

// A refused open must leave the filesystem untouched. The index file was
// already protected, but the directory chain was built before that check, so a
// mis-resolved path still got a tree created under it.
func TestNoCreateLeavesNoDirectories(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "does", "not", "exist")
	info := &mailbox.UserInfo{Username: "u@x.test", Home: home}

	idx := New(WithNoCreate()).OpenUser(info)
	t.Cleanup(func() { _ = idx.Close() })
	if _, err := idx.OpenFolder("INBOX", 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenFolder err = %v, want os.ErrNotExist", err)
	}

	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the refused open created %s (err=%v)", home, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the refused open left %v under the root", names)
	}
}
