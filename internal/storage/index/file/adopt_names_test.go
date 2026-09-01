package file_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// foreignStoreWithCyrillicFolders lays out a store of theirs holding two
// folders whose names are in their encoding, one inside the other.
func foreignStoreWithCyrillicFolders(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// "Вхідні" and "Вхідні/Робота" as the reference spells them on disk.
	for _, rel := range []string{
		filepath.Join("&BBIERQRWBDQEPQRW-", "dbox-Mails"),
		filepath.Join("&BBIERQRWBDQEPQRW-", "&BCAEPgQxBD4EQgQw-", "dbox-Mails"),
	} {
		dir := filepath.Join(home, "sdbox", "mailboxes", rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "dovecot.index.log"), dboxref.SdboxInboxLog(t), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func openIdx(home string) mailbox.UserIndex {
	return indexfile.New(indexfile.WithListUTF8(true)).
		OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
}

// The names are brought over when the store is opened, before anything lists
// it. Running this only from a folder conversion served the first listing in
// their encoding, with every subscribed folder unselectable (#1609).
func TestForeignNamesAreAdoptedBeforeAnythingLists(t *testing.T) {
	home := foreignStoreWithCyrillicFolders(t)
	idx := openIdx(home)
	defer idx.Close() //nolint:errcheck

	a, ok := idx.(mailbox.ForeignNameAdopter)
	if !ok {
		t.Fatal("the index does not offer to adopt foreign names")
	}
	if err := a.AdoptForeignNames(); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	root := filepath.Join(home, "sdbox", "mailboxes")
	for _, want := range []string{
		filepath.Join(root, "Вхідні"),
		filepath.Join(root, "Вхідні", "Робота"),
	} {
		if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
			t.Errorf("%s is not there after the pass: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "&BBIERQRWBDQEPQRW-")); !os.IsNotExist(err) {
		t.Errorf("the name in their encoding is still on disk: %v", err)
	}
}

// Two connections on one login is ordinary, and both run the pass. They must
// not race each other into a half-renamed tree, and the second must not fail
// because the first already did the work.
func TestTwoOpensAdoptingAtOnceLeaveOneCorrectTree(t *testing.T) {
	home := foreignStoreWithCyrillicFolders(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idx := openIdx(home)
			defer idx.Close() //nolint:errcheck
			errs[i] = idx.(mailbox.ForeignNameAdopter).AdoptForeignNames()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("open %d: %v", i, err)
		}
	}

	root := filepath.Join(home, "sdbox", "mailboxes")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "Вхідні" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the tree holds %v, want exactly one folder named Вхідні", names)
	}
	if _, err := os.Stat(filepath.Join(root, "Вхідні", "Робота")); err != nil {
		t.Errorf("the nested folder did not survive two passes: %v", err)
	}
}
