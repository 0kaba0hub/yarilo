package mailboxbuild

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The backstop only matters when the validation above it has been bypassed, so
// that is how it is tested: the driver is taken unwrapped, exactly as a caller
// that skipped mailboxbuild.ByDriver would have it.
//
// "The caller checked" is a promise, not a property. Since #1069 moved
// validation above the drivers, that promise is the only thing between a
// mistake and an account -- and #1063 is what it costs when the promise is not
// kept. This turns a silent loss into a refusal.
func TestDriversRefuseToDestroyTheirOwnRoot(t *testing.T) {
	sc := config.StorageConfig{}

	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			home := t.TempDir()
			// Unwrapped: byDriver, not ByDriver. Nothing validates the name.
			u := byDriver(driver, sc, nil).OpenUser(&mailbox.UserInfo{
				Username: "u@d.test", Home: home, Driver: driver, Separator: "/",
			})
			t.Cleanup(func() { _ = u.Close() })
			if err := u.Init(); err != nil {
				t.Fatal(err)
			}
			if err := u.Create("Keep"); err != nil {
				t.Fatal(err)
			}
			msg := "From: a@b\r\n\r\nkeep me\r\n"
			if _, _, _, err := u.Save("Keep", strings.NewReader(msg), 1, int64(len(msg)), nil, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			before := treeOf(t, home)

			// Names that resolve onto the root. "" does it on every driver;
			// "INBOX" does it on maildir, where INBOX *is* the mail root and
			// the driver's own checkName exempts the name -- which is exactly
			// how #1063 destroyed an account, and the case a backstop on the
			// path catches while a check on the name cannot.
			for _, name := range []string{"", "INBOX"} {
				if !rootName(driver, name) {
					// An ordinary folder on this driver; removing it is not
					// what this test is about, and doing it anyway would make
					// the tree comparison below meaningless.
					continue
				}
				if err := u.Delete(name); err == nil {
					t.Errorf("Delete(%q) was carried out on an unvalidated driver", name)
				}
				if err := u.Rename(name, "Elsewhere"); err == nil {
					t.Errorf("Rename(%q, ...) was carried out on an unvalidated driver", name)
				}
				if err := u.Rename("Keep", name); err == nil {
					t.Errorf("Rename(..., %q) was carried out — renaming onto the root buries the mailbox", name)
				}
			}

			if after := treeOf(t, home); after != before {
				t.Errorf("the mailbox changed:\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

// A real folder is still removable and renameable, or the guard would have
// disabled the operations rather than bounded them.
func TestDriversStillDeleteAndRenameOrdinaryFolders(t *testing.T) {
	sc := config.StorageConfig{}
	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			u := byDriver(driver, sc, nil).OpenUser(&mailbox.UserInfo{
				Username: "u@d.test", Home: t.TempDir(), Driver: driver, Separator: "/",
			})
			t.Cleanup(func() { _ = u.Close() })
			if err := u.Init(); err != nil {
				t.Fatal(err)
			}
			if err := u.Create("Archive"); err != nil {
				t.Fatal(err)
			}
			if err := u.Rename("Archive", "Archive2"); err != nil {
				t.Errorf("Rename: %v", err)
			}
			if err := u.Delete("Archive2"); err != nil {
				t.Errorf("Delete: %v", err)
			}
		})
	}
}

// rootName reports whether name resolves onto a root for this driver. On the
// dbox layouts INBOX is an ordinary folder under mailboxes/, so only the empty
// name reaches their root.
func rootName(driver, name string) bool {
	if name == "" {
		return true
	}
	return driver == "maildir" && name == "INBOX"
}
