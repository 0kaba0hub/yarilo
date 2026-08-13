package ftsstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// The declared storage type is what decides whether the durability call is
// issued -- not a comment, and not a guess about the mount. Both directions
// pinned: dropping the gate makes the nfs row fail, hard-coding the skip makes
// the local rows fail. Moved here from the engine with #1053: whether fsyncing
// a directory means anything is a property of the medium.
func TestStorageTypeDecidesDirSync(t *testing.T) {
	cases := []struct {
		storageType string
		want        bool
	}{
		{"", true},      // unset = local = do the extra work
		{"local", true}, // fsync is what makes the rename survive a crash
		{"nfs", false},  // metadata already committed by protocol; no-op
		{"NFS", false},  // the declared value is a name, not a byte sequence
	}
	for _, c := range cases {
		if got := NewPosix(fts.Layout{}, c.storageType).dirSyncUseful(); got != c.want {
			t.Errorf("storage type %q: dirSyncUseful() = %v, want %v", c.storageType, got, c.want)
		}
	}
}

// Sync must still answer for a directory that cannot be opened on a medium
// where the call is skipped: the skip is the answer, not a deferred failure.
func TestSyncOnNFSDoesNotTouchTheDirectory(t *testing.T) {
	p := NewPosix(fts.Layout{}, StorageTypeNFS)
	if err := p.Sync(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("Sync on nfs = %v, want nil", err)
	}
	local := NewPosix(fts.Layout{}, "local")
	if err := local.Sync(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("Sync on local reported success for a directory that does not exist")
	}
}

// The driver in fts_index_root selects the implementation. An unknown one is
// refused at startup, because a name with nothing behind it would otherwise be
// treated as a path and write the index into a directory named after the
// driver.
func TestDriverSelection(t *testing.T) {
	tests := []struct {
		name       string
		root       string
		wantDriver string
		wantRest   string
		wantErr    bool
	}{
		{"reference form", "posix:prefix=/srv/fts/%d/%n", "posix", "/srv/fts/%d/%n", false},
		{"location form", "posix:/srv/fts", "posix", "/srv/fts", false},
		{"bare path is posix", "/srv/fts", "posix", "/srv/fts", false},
		{"case is not part of the name", "POSIX:prefix=/srv/fts", "posix", "/srv/fts", false},
		{"unknown driver is refused", "s3:bucket=mail-fts", "s3", "bucket=mail-fts", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			driver, rest := DriverOf(tc.root)
			if driver != tc.wantDriver || rest != tc.wantRest {
				t.Errorf("DriverOf(%q) = (%q, %q), want (%q, %q)",
					tc.root, driver, rest, tc.wantDriver, tc.wantRest)
			}
			store, err := New(tc.root, fts.Layout{}, "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("unknown driver %q built a store instead of refusing", driver)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, ok := store.(*Posix); !ok {
				t.Errorf("driver %q built %T, want *Posix", driver, store)
			}
		})
	}
}

// A store built for a bare-path root still keys indexes the way its layout
// says, and adopts one left at a legacy location. The layout is the engine's;
// executing it on this medium is the store's.
func TestPrepareAdoptsALegacyLocation(t *testing.T) {
	root := t.TempDir()
	layout := fts.Layout{
		Dir: func(root string, _ fts.UserRef, mbox fts.MailboxRef) string {
			return filepath.Join(root, mbox.GUID, "idx")
		},
		Legacy: func(root string, _ fts.UserRef, mbox fts.MailboxRef) []string {
			return []string{filepath.Join(root, "old", mbox.Name, "idx")}
		},
	}
	user := fts.UserRef{Username: "u@test", IndexRoot: root}
	mbox := fts.MailboxRef{Name: "INBOX", GUID: "guid-1"}

	legacy := filepath.Join(root, "old", "INBOX", "idx")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := NewPosix(layout, "local")
	dir, err := p.Prepare(user, mbox)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := filepath.Join(root, "guid-1", "idx"); dir != want {
		t.Errorf("Prepare = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
		t.Errorf("the legacy index was not adopted: %v", err)
	}
	// The shell the move left behind goes, or "did the migration run" cannot
	// be read off the tree (#1195).
	if _, err := os.Stat(filepath.Join(root, "old")); !os.IsNotExist(err) {
		t.Errorf("empty parents of the legacy location survived: %v", err)
	}
}

// Prepare must not create anything: an engine that only reads a mailbox it has
// never indexed would otherwise leave a directory behind for every folder it
// looked at.
func TestPrepareCreatesNothing(t *testing.T) {
	root := t.TempDir()
	p := NewPosix(fts.Layout{
		Dir: func(root string, _ fts.UserRef, mbox fts.MailboxRef) string {
			return filepath.Join(root, mbox.GUID, "idx")
		},
	}, "local")
	dir, err := p.Prepare(fts.UserRef{IndexRoot: root}, fts.MailboxRef{Name: "INBOX", GUID: "g"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Prepare created %s (stat err %v)", dir, err)
	}
	if err := p.Create(dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Create left nothing at %s: %v", dir, err)
	}
}
