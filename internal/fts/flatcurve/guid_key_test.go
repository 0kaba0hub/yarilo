//go:build flatcurve

package flatcurve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// A rename keeps the folder's GUID, so a GUID-keyed index survives it with
// nothing to implement -- which is the point of the key (#1183).
//
// The distinguishing part is that NOTHING is indexed after the rename: a
// search that merely works could have been served by a silent rebuild, and a
// silent rebuild IS the defect. The renamed mailbox is never handed to
// BeginUpdate again, so a hit can only come from the index that already
// existed.
func TestRenameKeepsTheIndexBecauseTheKeyIsTheGUID(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	before := fts.MailboxRef{GUID: "g-stable", Name: "Projects", UIDValidity: 1}
	indexDocIn(t, ui, before, 1, nil, []string{"unrepeatable-token"})
	if err := ui.Refresh(); err != nil {
		t.Fatal(err)
	}

	// The rename: same folder, same GUID, new name. No reindexing follows.
	after := fts.MailboxRef{GUID: "g-stable", Name: "Archive/2026", UIDValidity: 1}
	res, err := ui.Lookup(after, bodyQuery("unrepeatable-token"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 1 {
		t.Errorf("the renamed mailbox lost its index: %v", res.Definite)
	}

	// And the two names resolve to ONE directory, which is what makes the
	// above true rather than lucky.
	if d1, d2 := ui.(*userIndex).state(before).dir, ui.(*userIndex).state(after).dir; d1 != d2 {
		t.Errorf("names resolve to different directories: %s vs %s", d1, d2)
	}
}

// A driver change moves the mail tree, not the folder's identity, so it must
// not orphan the index either.
func TestDriverChangeKeepsTheIndex(t *testing.T) {
	root := t.TempDir()
	mbox := fts.MailboxRef{GUID: "g-same", Name: "INBOX", UIDValidity: 1}

	maildir := fts.UserRef{Username: "u@test", IndexRoot: root, Driver: "maildir", Separator: "."}
	ui1, err := New(Options{}).OpenUser(t.Context(), maildir)
	if err != nil {
		t.Fatal(err)
	}
	indexDocIn(t, ui1, mbox, 1, nil, []string{"survives-the-driver"})
	if err := ui1.Refresh(); err != nil {
		t.Fatal(err)
	}
	ui1.Close() //nolint:errcheck

	mdbox := fts.UserRef{Username: "u@test", IndexRoot: root, Driver: "mdbox", Separator: "/"}
	ui2, err := New(Options{}).OpenUser(t.Context(), mdbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui2.Close() }) //nolint:errcheck
	res, err := ui2.Lookup(mbox, bodyQuery("survives-the-driver"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 {
		t.Errorf("the index was orphaned by the driver change: %v", res.Definite)
	}
}

// An index written under an older layout is adopted, not abandoned: both
// name-keyed layouts are looked for, the data moves to the GUID path, and
// the directories it sat in go with it.
//
// The mdbox case is the one that distinguishes: its layout nests the index
// two levels down (mailboxes/<name>/dbox-Mails/), so removing the moved
// directory alone leaves a shell that answers "not migrated yet" to anyone
// looking (#1195). A flat fixture cannot show that -- there the moved
// directory IS the whole chain.
func TestOlderLayoutsAreMigrated(t *testing.T) {
	for _, tc := range []struct {
		name string
		user fts.UserRef
		dir  func(root string) string
		gone []func(root string) string // must not survive the migration
	}{
		{
			name: "driver-aware layout, nested (mdbox)",
			user: fts.UserRef{Username: "u@test", Driver: "mdbox", Separator: "/"},
			dir: func(root string) string {
				return filepath.Join(root, "mailboxes", "Sent", "dbox-Mails", Label)
			},
			gone: []func(string) string{
				func(r string) string { return filepath.Join(r, "mailboxes", "Sent", "dbox-Mails") },
				func(r string) string { return filepath.Join(r, "mailboxes", "Sent") },
			},
		},
		{
			name: "driver-aware layout (maildir)",
			user: fts.UserRef{Username: "u@test", Driver: "maildir", Separator: "."},
			dir:  func(root string) string { return filepath.Join(root, ".Sent", Label) },
			gone: []func(string) string{func(r string) string { return filepath.Join(r, ".Sent") }},
		},
		{
			name: "flat layout",
			user: fts.UserRef{Username: "u@test", Driver: "maildir", Separator: "."},
			dir:  func(root string) string { return filepath.Join(root, "Sent", Label) },
			gone: []func(string) string{func(r string) string { return filepath.Join(r, "Sent") }},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			user := tc.user
			user.IndexRoot = root
			mbox := fts.MailboxRef{GUID: "g-sent", Name: "Sent", UIDValidity: 1}

			// Seed an index at the old location by writing there directly.
			old := tc.dir(root)
			if err := os.MkdirAll(old, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(old, "marker"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}

			ui, err := New(Options{}).OpenUser(t.Context(), user)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ui.Close() }) //nolint:errcheck
			newDir := ui.(*userIndex).state(mbox).dir

			if _, serr := os.Stat(filepath.Join(newDir, "marker")); serr != nil {
				t.Errorf("the old index was not adopted at the GUID path: %v", serr)
			}
			if _, serr := os.Stat(old); !os.IsNotExist(serr) {
				t.Errorf("the old directory is still there: %v", serr)
			}
			for _, g := range tc.gone {
				if _, serr := os.Stat(g(root)); !os.IsNotExist(serr) {
					t.Errorf("the migration left %s behind, which reads as "+
						"\"not migrated yet\" to anyone looking", g(root))
				}
			}
			// The index root itself is never climbed past.
			if _, serr := os.Stat(root); serr != nil {
				t.Errorf("the index root was removed: %v", serr)
			}
		})
	}
}

// The climb stops at the first directory that is not empty: a user whose
// other mailboxes have not been migrated yet must keep the shell, or the
// prune would take their indexes with it.
func TestMigrationKeepsAParentThatStillHoldsAnotherIndex(t *testing.T) {
	root := t.TempDir()
	user := fts.UserRef{Username: "u@test", IndexRoot: root, Driver: "mdbox", Separator: "/"}
	mbox := fts.MailboxRef{GUID: "g-sent", Name: "Sent", UIDValidity: 1}

	old := filepath.Join(root, "mailboxes", "Sent", "dbox-Mails", Label)
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sibling mailbox still on the old layout, untouched by this migration.
	sibling := filepath.Join(root, "mailboxes", "Drafts", "dbox-Mails", Label)
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}

	ui, err := New(Options{}).OpenUser(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck
	_ = ui.(*userIndex).state(mbox).dir

	if _, serr := os.Stat(sibling); serr != nil {
		t.Errorf("the prune took an unmigrated mailbox's index with it: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(root, "mailboxes")); serr != nil {
		t.Errorf("the shared parent was removed while still in use: %v", serr)
	}
}
