package mailboxbuild

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The property the feature exists for: the name the client wrote is the name
// the client gets back, on every driver, whatever the layout does underneath.
//
// Without escaping, "Invoices.2026" on maildir is written flat and listed as
// "Invoices/2026" -- two mailboxes where the client made one, and the
// substitution is invisible unless the client LISTs (#1078).
func TestEscapedNamesRoundTripThroughListFolders(t *testing.T) {
	names := []string{
		"Invoices.2026",
		"example.com",
		"a^b",  // the escape character itself: this is what keeps decoding unambiguous
		"cur",  // a name the layout owns
		"Work", // ordinary, must be untouched
	}

	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		for _, utf8 := range []bool{true, false} {
			t.Run(label(driver, utf8), func(t *testing.T) {
				sc := config.StorageConfig{
					MailboxListValidateFSNames:   true,
					MailboxListStorageEscapeChar: "^",
					MailboxListUTF8:              utf8,
					MailboxListNormalizeToNFC:    true,
				}
				u := ByDriver(driver, sc, nil).OpenUser(&mailbox.UserInfo{
					Username:          "u@d.test",
					Home:              t.TempDir(),
					Driver:            driver,
					Separator:         "/",
					StorageEscapeChar: "^",
				})
				t.Cleanup(func() { _ = u.Close() })
				if err := u.Init(); err != nil {
					t.Fatal(err)
				}

				for _, name := range names {
					if err := u.Create(name); err != nil {
						t.Fatalf("Create(%q): %v", name, err)
					}
				}
				// Hierarchy the client asked for must still be hierarchy.
				if err := u.Create("Parent/Child"); err != nil {
					t.Fatalf("Create nested: %v", err)
				}

				listed, err := u.ListFolders()
				if err != nil {
					t.Fatal(err)
				}
				got := map[string]bool{}
				for _, f := range listed {
					got[f.Name] = true
				}
				for _, want := range append(names, "Parent/Child") {
					if !got[want] {
						t.Errorf("%q was created but listed as something else; got %v", want, keys(got))
					}
				}
			})
		}
	}
}

// The same folder is one mailbox on every driver. Without escaping it is one
// mailbox on dbox and two levels on maildir, so a migration that changes format
// silently renames it -- which is the half of #1078 that documentation cannot
// fix.
func TestADottedNameIsOneMailboxOnEveryDriver(t *testing.T) {
	sc := config.StorageConfig{
		MailboxListValidateFSNames:   true,
		MailboxListStorageEscapeChar: "^",
		MailboxListUTF8:              true,
	}
	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		u := ByDriver(driver, sc, nil).OpenUser(&mailbox.UserInfo{
			Username: "u@d.test", Home: t.TempDir(), Driver: driver,
			Separator: "/", StorageEscapeChar: "^",
		})
		t.Cleanup(func() { _ = u.Close() })
		if err := u.Init(); err != nil {
			t.Fatal(err)
		}
		if err := u.Create("Invoices.2026"); err != nil {
			t.Fatalf("%s: Create: %v", driver, err)
		}
		listed, err := u.ListFolders()
		if err != nil {
			t.Fatal(err)
		}
		var found int
		for _, f := range listed {
			switch f.Name {
			case "Invoices.2026":
				found++
			case "Invoices", "Invoices/2026":
				t.Errorf("%s: listed %q — the name was reinterpreted as nesting", driver, f.Name)
			}
		}
		if found != 1 {
			t.Errorf("%s: the folder is not one mailbox: %v", driver, listed)
		}
	}
}

// With no escape character configured nothing changes, which is what makes the
// default safe for existing installations.
func TestWithoutAnEscapeCharTheOldBehaviourStands(t *testing.T) {
	sc := config.StorageConfig{MailboxListValidateFSNames: true}
	u := ByDriver("maildir", sc, nil).OpenUser(&mailbox.UserInfo{
		Username: "u@d.test", Home: t.TempDir(), Driver: "maildir", Separator: "/",
	})
	t.Cleanup(func() { _ = u.Close() })
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	if err := u.Create("Invoices.2026"); err != nil {
		t.Fatal(err)
	}
	listed, err := u.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range listed {
		if f.Name == "Invoices/2026" {
			return // the documented (and now documented-as-such) behaviour
		}
	}
	t.Errorf("expected the unescaped name to still list as nesting, got %v", listed)
}

// Escaping supersedes the refusals that exist because the name could not be
// represented -- otherwise the validator above rejects exactly the names
// escaping was added to allow.
func TestEscapingSupersedesTheRepresentabilityRefusals(t *testing.T) {
	rules := mailbox.NameRules{
		ValidateFSNames:       true,
		RefuseLayoutSeparator: true,
		ReservedSegments:      []string{"cur", "new", "tmp"},
		StorageEscapeChar:     "^",
	}
	for _, name := range []string{"Invoices.2026", "cur"} {
		if err := mailbox.ValidateName(name, "/", ".", rules); err != nil {
			t.Errorf("ValidateName(%q) = %v; escaping stores it rather than refusing it", name, err)
		}
	}
	// Traversal is still refused: ".." is not a name anyone means to own, and
	// escaping it would create a folder no client asked for.
	if err := mailbox.ValidateName("../victim", "/", ".", rules); err == nil {
		t.Error("a traversal name was accepted because escaping was configured")
	}
}

func label(driver string, utf8 bool) string {
	if utf8 {
		return driver + "/utf8"
	}
	return driver + "/modutf7"
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
