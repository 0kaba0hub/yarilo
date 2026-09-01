package file_test

import (
	"errors"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An mdbox folder the record knows, opened with its index gone, is refused
// rather than served empty. Until the record existed this could not be
// detected: an mdbox folder directory holds no message files, so a directory
// without an index looks exactly like a folder just created (#1608).
func TestAnMdboxFolderWhoseIndexIsLostIsRefused(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"}

	idx := indexfile.New().OpenUser(info)
	before, err := idx.OpenFolder("Work", 1788252508)
	if err != nil {
		t.Fatal(err)
	}
	idx.Close() //nolint:errcheck

	dropFolderIndexes(t, home)

	idx2 := indexfile.New().OpenUser(info)
	defer idx2.Close() //nolint:errcheck
	f, err := idx2.OpenFolder("Work", 1788299999)
	if err == nil {
		t.Fatalf("the folder opened with %d messages and uidvalidity %d; its mail is in the storage",
			f.Messages, f.UIDValidity)
	}
	if !errors.Is(err, mailbox.ErrIndexLost) {
		t.Fatalf("the refusal is %v, and it should say what happened", err)
	}
	// An operator has to learn which folder, what it was, and what to do.
	for _, want := range []string{"Work", "1788252508", "mailboxes stopped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	_ = before
}

// A folder older than the record has no entry, and the ambiguity stays: it
// opens as it always did. Widening the trigger past what the record can prove
// would refuse folders that really are new.
func TestAnMdboxFolderTheRecordNeverKnewStillOpens(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	if _, err := idx.OpenFolder("Work", 1788252508); err != nil {
		t.Fatalf("a folder with no entry was refused: %v", err)
	}
}

// Creating a folder is not opening one: the create path must be untouched even
// under a name the record still knows.
func TestCreatingAnMdboxFolderIsNotRefused(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"}
	idx := indexfile.New().OpenUser(info)

	first, err := idx.OpenFolder("Work", 1788252508)
	if err != nil {
		t.Fatal(err)
	}
	idx.Close() //nolint:errcheck
	dropFolderIndexes(t, home)

	// A fresh handle, the way a restart reaches the same tree: an open one
	// answers from the folder it already holds.
	idx = indexfile.New().OpenUser(info)
	fc, ok := idx.(mailbox.FolderCreator)
	if !ok {
		t.Fatal("the index does not offer to create a folder")
	}
	defer idx.Close() //nolint:errcheck
	again, err := fc.CreateFolder("Work", 1788299999)
	if err != nil {
		t.Fatalf("a create was refused: %v", err)
	}
	if again.UIDValidity == first.UIDValidity {
		t.Errorf("the created folder inherited uidvalidity %d", first.UIDValidity)
	}
}
