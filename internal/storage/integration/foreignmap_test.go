package integration_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Two sessions opening two different folders of a foreign store at the same
// time convert its map once between them.
//
// The folder lock does not order them: it is per folder, and these are two
// folders. Only the map's own lock does, which is why the "has this map been
// converted yet" check and the append that follows it have to be one section
// inside it. Checked outside, both sessions find an empty map and import every
// record twice, with a doubled refcount and a correspondence that resolves to
// whichever copy landed first (#1524).
//
// Run with a real lock service, because without one there is no cross-instance
// ordering to test and the result would say nothing about a deployment.
func TestConcurrentFolderOpensConvertTheForeignMapOnce(t *testing.T) {
	dial := embeddedLocksForSaveTest(t)
	home := t.TempDir()
	root := filepath.Join(home, "mdbox")
	storage := filepath.Join(root, "storage")
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, b []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Two folders of theirs over one store, so both openers need the same map.
	for _, folder := range []string{"INBOX", "Archive"} {
		dir := filepath.Join(root, "mailboxes", folder, "dbox-Mails")
		write(filepath.Join(dir, "dovecot.index"), dboxref.IndexBase(t))
		write(filepath.Join(dir, "dovecot.index.log"), dboxref.IndexLog(t))
		write(filepath.Join(dir, "dovecot.index.log.2"), dboxref.IndexLogRotated(t))
	}
	write(filepath.Join(storage, "dovecot.map.index.log"), dboxref.MapLog(t))
	write(filepath.Join(storage, "m.1"), dboxref.StoreFile(t))

	const user = "u1@d00001.test"
	idx := indexfile.New(indexfile.WithLocker(dial())).
		OpenUser(&mailbox.UserInfo{Username: user, Home: home, Driver: "mdbox"})
	defer idx.Close() //nolint:errcheck

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, folder := range []string{"INBOX", "Archive"} {
		wg.Add(1)
		go func(i int, folder string) {
			defer wg.Done()
			_, errs[i] = idx.OpenFolder(folder, 0)
		}(i, folder)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("opener %d: %v", i, err)
		}
	}

	m, err := mdboxmap.Open(storage, user, mdboxmap.WithLocker(dial()))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close() //nolint:errcheck

	// Their map holds five referenced records. Ten is both openers importing it.
	const want = 5
	recs := m.Records()
	if len(recs) != want {
		t.Fatalf("our map holds %d records, and theirs holds %d", len(recs), want)
	}
	for _, r := range recs {
		if r.RefCount != 1 {
			t.Errorf("map uid %d has refcount %d, want 1", r.UID, r.RefCount)
		}
	}
}
