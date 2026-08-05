package mailboxbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Every driver built here refuses a name that resolves outside the mailbox,
// whatever the caller is. Before this, only maildir checked: mdbox and sdbox
// wrote wherever the name pointed, and Sieve fileinto takes that name from the
// user's own script, so delivery could land in another account without any IMAP
// command being issued (#1069).
//
// The write is asserted on disk rather than through the returned error: a
// driver that returned an error *after* writing would satisfy a check of the
// error alone.
func TestEveryDriverRefusesNamesOutsideTheMailbox(t *testing.T) {
	sc := config.StorageConfig{
		MailboxListValidateFSNames:  true,
		MailboxListReservedSegments: []string{"cur", "new", "tmp", "dbox-Mails"},
	}

	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			base := t.TempDir()
			victimHome := filepath.Join(base, "victim@d.test")
			attackerHome := filepath.Join(base, "attacker@d.test")
			for _, h := range []string{victimHome, attackerHome} {
				if err := os.MkdirAll(h, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			u := ByDriver(driver, sc, nil).OpenUser(&mailbox.UserInfo{
				Username:  "attacker@d.test",
				Home:      attackerHome,
				Driver:    driver,
				Separator: "/",
			})
			t.Cleanup(func() { _ = u.Close() })
			if err := u.Init(); err != nil {
				t.Fatal(err)
			}

			victimBefore := treeOf(t, victimHome)

			for _, name := range []string{
				"..",
				"../victim@d.test",
				"../../victim@d.test/mdbox/mailboxes/PLANTED",
				"evil/../../../victim@d.test/Maildir/PLANTED",
				"",
			} {
				if err := u.Create(name); err == nil {
					t.Errorf("Create(%q) was accepted", name)
				}
				msg := "From: a@b\r\n\r\nplanted\r\n"
				if _, _, _, err := u.Save(name, strings.NewReader(msg), 1, int64(len(msg)), nil, [16]byte{}); err == nil {
					t.Errorf("Save(%q) was accepted — this is the Sieve fileinto path", name)
				}
				if err := u.Delete(name); err == nil {
					t.Errorf("Delete(%q) was accepted", name)
				}
			}

			if after := treeOf(t, victimHome); after != victimBefore {
				t.Errorf("the other account changed:\nbefore: %s\nafter:  %s", victimBefore, after)
			}
		})
	}
}

// An ordinary folder still works on every driver — otherwise the test above
// passes on a backend that refuses everything.
func TestOrdinaryFolderStillWorksOnEveryDriver(t *testing.T) {
	sc := config.StorageConfig{
		MailboxListValidateFSNames:  true,
		MailboxListReservedSegments: []string{"cur", "new", "tmp", "dbox-Mails"},
	}
	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			u := ByDriver(driver, sc, nil).OpenUser(&mailbox.UserInfo{
				Username:  "u@d.test",
				Home:      t.TempDir(),
				Driver:    driver,
				Separator: "/",
			})
			t.Cleanup(func() { _ = u.Close() })
			if err := u.Init(); err != nil {
				t.Fatal(err)
			}
			if err := u.Create("Archive"); err != nil {
				t.Fatalf("Create(Archive): %v", err)
			}
			msg := "From: a@b\r\n\r\nhello\r\n"
			if _, _, _, err := u.Save("Archive", strings.NewReader(msg), 1, int64(len(msg)), nil, [16]byte{}); err != nil {
				t.Errorf("Save(Archive): %v", err)
			}
			if err := u.Delete("Archive"); err != nil {
				t.Errorf("Delete(Archive): %v", err)
			}
		})
	}
}

func treeOf(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel + "\n")
		return nil
	})
	return b.String()
}

// A dotted folder name keeps working with the default rules. The
// separator-conflict rule is real but retroactive against ordinary names, so it
// is off unless a deployment turns it on: switching it on by default would make
// an existing "example.com" folder visible in LIST and impossible to select.
func TestDottedFolderNamesSurviveTheDefaultRules(t *testing.T) {
	sc := config.StorageConfig{
		MailboxListValidateFSNames:  true,
		MailboxListReservedSegments: []string{"cur", "new", "tmp", "dbox-Mails"},
	}
	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			u := ByDriver(driver, sc, nil).OpenUser(&mailbox.UserInfo{
				Username:  "u@d.test",
				Home:      t.TempDir(),
				Driver:    driver,
				Separator: "/",
			})
			t.Cleanup(func() { _ = u.Close() })
			if err := u.Init(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"example.com", "Invoices.2026", "lists.golang-nuts"} {
				if err := u.Create(name); err != nil {
					t.Errorf("Create(%q): %v — an existing folder would become unselectable", name, err)
				}
				ok, err := u.FolderExists(name)
				if err != nil || !ok {
					t.Errorf("FolderExists(%q) = %v, %v — visible in LIST but not selectable", name, ok, err)
				}
			}
		})
	}
}

// With the rule on, the same names are refused -- the collision it names is
// real, and a deployment that enables it gets what it asked for.
func TestDottedFolderNamesAreRefusedWhenTheRuleIsOn(t *testing.T) {
	sc := config.StorageConfig{
		MailboxListValidateFSNames:       true,
		MailboxListRefuseLayoutSeparator: true,
	}
	u := ByDriver("maildir", sc, nil).OpenUser(&mailbox.UserInfo{
		Username:  "u@d.test",
		Home:      t.TempDir(),
		Driver:    "maildir",
		Separator: "/",
	})
	t.Cleanup(func() { _ = u.Close() })
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	if err := u.Create("example.com"); err == nil {
		t.Error("with the separator rule on, Create(\"example.com\") was accepted")
	}
	if err := u.Create("Work/2026"); err != nil {
		t.Errorf("Create(\"Work/2026\"): %v — ordinary nesting must still work", err)
	}
}
