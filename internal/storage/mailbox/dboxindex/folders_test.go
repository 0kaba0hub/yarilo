package dboxindex_test

import (
	"testing"
	"testing/fstest"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
)

// referenceLayout is the mailboxes directory of a store the reference wrote,
// recorded from it rather than invented:
//
//	mailboxes/INBOX/dbox-Mails
//	mailboxes/Archive/dbox-Mails
//	mailboxes/Archive/2026/dbox-Mails
//	mailboxes/&BBIERQRWBDQEPQRW-/dbox-Mails               ("Вхідні")
//	mailboxes/&BBIERQRWBDQEPQRW-/&BCAEPgQxBD4EQgQw-/...   ("Вхідні/Робота")
//
// A map filesystem rather than files in the tree: what is under test is the
// shape of the directories, and the index files inside them are read by tests
// that have their own fixtures.
func referenceLayout() fstest.MapFS {
	return fstest.MapFS{
		"INBOX/dbox-Mails/dovecot.index":                                 {},
		"Archive/dbox-Mails/dovecot.index":                               {},
		"Archive/2026/dbox-Mails/dovecot.index":                          {},
		"&BBIERQRWBDQEPQRW-/dbox-Mails/dovecot.index":                    {},
		"&BBIERQRWBDQEPQRW-/&BCAEPgQxBD4EQgQw-/dbox-Mails/dovecot.index": {},
	}
}

// The folders of a store, from its directories.
//
// Three things this has to get right, and the layout was chosen so that each
// one fails separately:
//
//   - a folder is a directory holding dbox-Mails, so dbox-Mails itself is not a
//     folder and must not appear;
//   - a nested folder sits inside its parent, beside the parent's own
//     dbox-Mails, so the walk cannot stop at the first folder it finds;
//   - a non-ASCII name is modified UTF-7 on disk and must come back as itself,
//     at every level.
//
// What this cannot show is that the path may be decoded whole: modified base64
// uses ',' where base64 uses '/', so a separator never appears inside an
// encoded run and no input distinguishes level-by-level decoding from decoding
// the join. The code decodes the join and says why.
func TestTheFoldersOfAReferenceStore(t *testing.T) {
	got, err := dboxindex.WalkFolders(referenceLayout())
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	want := []string{"Archive", "Archive/2026", "INBOX", "Вхідні", "Вхідні/Робота"}
	if len(got) != len(want) {
		var names []string
		for _, f := range got {
			names = append(names, f.Name)
		}
		t.Fatalf("found %v, want %v", names, want)
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("folder %d is %q, want %q", i, got[i].Name, w)
		}
	}

	for _, f := range got {
		if f.Path == "" {
			t.Errorf("folder %q has no path, so its index cannot be found", f.Name)
		}
	}
}

// A directory that holds folders but is not one is walked into and not listed.
//
// The reference makes these when a client creates a/b without a: selecting it
// would show an empty mailbox that is not there, and a client is supposed to be
// told it cannot be selected.
func TestAContainerIsNotAFolder(t *testing.T) {
	got, err := dboxindex.WalkFolders(fstest.MapFS{
		"Parent/Child/dbox-Mails/dovecot.index": {},
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Parent/Child" {
		var names []string
		for _, f := range got {
			names = append(names, f.Name)
		}
		t.Errorf("found %v, want only [Parent/Child]: Parent holds a folder but is not one", names)
	}
}
