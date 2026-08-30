package file_test

import (
	"os"
	"path/filepath"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The index tree spells a folder the way the mail tree does.
//
// Both follow mailbox_list_utf8, and the index used to ignore it: with the
// deployment writing modified UTF-7 the mail went to &BBIERQRWBDQEPQRW- and the
// index to Вхідні, so the two halves of one folder sat under two names and
// neither found the other's (#1586).
func TestTheIndexTreeFollowsTheConfiguredNameEncoding(t *testing.T) {
	const (
		folder  = "Вхідні"
		encoded = "&BBIERQRWBDQEPQRW-"
	)
	tests := []struct {
		name string
		utf8 bool
		want string
	}{
		{name: "utf-8 names on disk", utf8: true, want: folder},
		{name: "their encoding on disk", utf8: false, want: encoded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			idx := indexfile.New(indexfile.WithListUTF8(tc.utf8)).
				OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"})
			defer idx.Close() //nolint:errcheck
			if _, err := idx.OpenFolder(folder, 1); err != nil {
				t.Fatalf("open: %v", err)
			}
			want := filepath.Join(home, "mailboxes", tc.want, "dbox-Mails", "yarilo.index")
			if _, err := os.Stat(want); err != nil {
				t.Errorf("no index at %s: %v", want, err)
			}
			// And not under the other spelling, so a failure names which way it
			// went rather than only that it went.
			other := folder
			if tc.utf8 {
				other = encoded
			}
			if _, err := os.Stat(filepath.Join(home, "mailboxes", other)); !os.IsNotExist(err) {
				t.Errorf("the index also appeared under %q", other)
			}
		})
	}
}
