package file_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A foreign sdbox store is refused, not opened as new.
//
// Adopting one is separate work: there is no map to convert and a different
// question to answer about naming. What made this worth a refusal rather than a
// skip is what the skip produced -- the folder opened with a fresh index beside
// theirs and **no messages at all**, because nothing scans a dbox directory into
// an index the way the maildir sync does. The mail sits on disk, the mailbox
// looks healthy and empty, and nothing says why (#1592).
func TestAForeignSdboxStoreIsRefused(t *testing.T) {
	home := t.TempDir()
	// The sdbox driver roots at <home>/sdbox, so that is where a store of theirs
	// sits for a deployment with no mail_path of its own.
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"u.1":               dboxref.SdboxShort(t),
		"dovecot.index":     dboxref.IndexBase(t),
		"dovecot.index.log": dboxref.IndexLog(t),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer idx.Close() //nolint:errcheck

	_, err := idx.OpenFolder("INBOX", 0)
	if err == nil {
		t.Fatal("a folder holding a foreign sdbox index opened clean")
	}
	if !errors.Is(err, indexfile.ErrSdboxAdoptionUnsupported) {
		t.Errorf("the refusal is %v, and it should name what is unsupported", err)
	}
	// An operator has to learn where and what to do instead.
	for _, want := range []string{dir, "--src dbox-ref"} {
		if !contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// Nothing of theirs was touched, and nothing of ours was left behind to
	// make the next open look like an ordinary empty folder.
	for _, name := range []string{"u.1", "dovecot.index", "dovecot.index.log"} {
		if _, serr := os.Stat(filepath.Join(dir, name)); serr != nil {
			t.Errorf("%s did not survive the refusal: %v", name, serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "yarilo.index")); !os.IsNotExist(serr) {
		t.Errorf("our index was written although the folder was refused: %v", serr)
	}
}

// A folder of ours under the sdbox driver still opens: the refusal is about a
// foreign index being there, not about the driver.
func TestAnOrdinarySdboxFolderStillOpens(t *testing.T) {
	home := t.TempDir()
	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"})
	defer idx.Close() //nolint:errcheck
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("an ordinary sdbox folder was refused: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}
