package dboxconv_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
)

// The four combinations of "what this deployment writes" and "what is on disk".
// Two of them are no-ops, and they matter as much as the other two: a pass that
// renamed a store already in the right encoding would rewrite a tree for
// nothing, and one that renamed ASCII would rewrite every store there is.
func TestAdoptNamesBringsTheDiskToTheConfiguredEncoding(t *testing.T) {
	const (
		encoded = "&BBIERQRWBDQEPQRW-" // Вхідні
		child   = "&BCAEPgQxBD4EQgQw-" // Робота
		plain   = "Вхідні"
		kid     = "Робота"
	)
	tests := []struct {
		name    string
		utf8    bool
		onDisk  []string
		want    []string
		renames int
	}{
		{
			name:    "utf8 deployment, their encoding on disk",
			utf8:    true,
			onDisk:  []string{"INBOX", encoded, filepath.Join(encoded, child)},
			want:    []string{"INBOX", plain, filepath.Join(plain, kid)},
			renames: 2,
		},
		{
			name:    "modified utf-7 deployment, utf-8 on disk",
			utf8:    false,
			onDisk:  []string{"INBOX", plain, filepath.Join(plain, kid)},
			want:    []string{"INBOX", encoded, filepath.Join(encoded, child)},
			renames: 2,
		},
		{
			name:    "utf8 deployment, utf-8 already",
			utf8:    true,
			onDisk:  []string{"INBOX", plain, filepath.Join(plain, kid)},
			want:    []string{"INBOX", plain, filepath.Join(plain, kid)},
			renames: 0,
		},
		{
			name:    "modified utf-7 deployment, their encoding already",
			utf8:    false,
			onDisk:  []string{"INBOX", encoded, filepath.Join(encoded, child)},
			want:    []string{"INBOX", encoded, filepath.Join(encoded, child)},
			renames: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "mailboxes")
			for _, d := range tc.onDisk {
				if err := os.MkdirAll(filepath.Join(root, d, "dbox-Mails"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			n, err := dboxconv.AdoptNames(root, tc.utf8)
			if err != nil {
				t.Fatalf("adopt: %v", err)
			}
			if n != tc.renames {
				t.Errorf("renamed %d directories, want %d", n, tc.renames)
			}
			got := treeOf(t, root)
			sort.Strings(tc.want)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("tree is %v, want %v", got, tc.want)
			}
			// The message directories are still where the folders are.
			for _, d := range tc.want {
				if _, err := os.Stat(filepath.Join(root, d, "dbox-Mails")); err != nil {
					t.Errorf("%s lost its dbox-Mails: %v", d, err)
				}
			}
		})
	}
}

// A pass that stopped halfway is finished by the next one: renaming is
// idempotent per directory, so what already carries the right name is skipped
// and what does not is done now.
func TestAdoptNamesFinishesAPartialPass(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mailboxes")
	// Two different folders: one already renamed by the pass that stopped, one
	// not. (Both spellings of the *same* folder is a different case -- a merge,
	// refused below.)
	const encoded = "&BCAEPgQxBD4EQgQw-" // Робота
	for _, d := range []string{"Вхідні", encoded} {
		if err := os.MkdirAll(filepath.Join(root, d, "dbox-Mails"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// One already carries the new name, one does not: the state a crash between
	// two renames leaves behind.
	n, err := dboxconv.AdoptNames(root, true)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if n != 1 {
		t.Errorf("renamed %d, want 1 -- the one still in their encoding", n)
	}
	got := treeOf(t, root)
	if len(got) != 2 || got[0] != "Вхідні" || got[1] != "Робота" {
		t.Errorf("tree is %v, want both folders in this deployment's encoding", got)
	}
}

// A name that cannot be read as theirs is left exactly as it is: it may be what
// a user typed.
func TestAdoptNamesLeavesWhatItCannotDecode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mailboxes")
	const odd = "&notbase64$$"
	if err := os.MkdirAll(filepath.Join(root, odd, "dbox-Mails"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := dboxconv.AdoptNames(root, true)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if n != 0 {
		t.Errorf("renamed %d, and nothing there decodes as theirs", n)
	}
	if got := treeOf(t, root); len(got) != 1 || got[0] != odd {
		t.Errorf("tree is %v, want %q untouched", got, odd)
	}
}

// Two folders must never become one.
func TestAdoptNamesRefusesToMergeOntoAnExistingName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mailboxes")
	for _, d := range []string{"Вхідні", "&BBIERQRWBDQEPQRW-"} {
		if err := os.MkdirAll(filepath.Join(root, d, "dbox-Mails"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Both would end up as "Вхідні" under a modified-UTF-7 deployment... the
	// other way round: encoding the plain one lands on the encoded one.
	if _, err := dboxconv.AdoptNames(root, false); err == nil {
		t.Error("two folders were merged into one name")
	}
	if got := treeOf(t, root); len(got) != 2 {
		t.Errorf("tree is %v; nothing should have been lost", got)
	}
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() || p == root || info.Name() == "dbox-Mails" {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}
