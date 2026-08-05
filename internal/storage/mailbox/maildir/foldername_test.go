package maildir

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A maildir mailbox is its own root: INBOX is the mail directory and subfolders
// are `.name` siblings inside it. So a name contributing nothing to the path
// resolves to the mailbox itself, and a name of "." resolves above it.
//
// Delete on the first removed every message and the index, which is why the
// mailbox came back with a new UIDVALIDITY rather than merely empty; Delete on
// the second removed the user's home directory. Both were reachable from any
// caller that passed a folder name through without checking it (#1063).
//
// dbox survives the same names because every folder lives under a subdirectory,
// which is what made this look driver-specific rather than like a missing
// check.
func TestDestructiveFolderNamesAreRefused(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/..", "../x", "a/./b", "sub/.."} {
		t.Run("name="+name, func(t *testing.T) {
			home := t.TempDir()
			u := openTestUser(t, home)

			if _, _, _, err := u.Save("INBOX", strings.NewReader("Subject: a\r\n\r\nbody\r\n"),
				1, 20, nil, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, "Maildir")
			if _, err := os.Stat(filepath.Join(root, "cur")); err != nil {
				root = filepath.Join(home, "INBOX")
			}

			if err := u.Delete(name); err == nil {
				t.Errorf("Delete(%q) was accepted", name)
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatalf("the mailbox root is gone after Delete(%q): %v", name, err)
			}
			if _, err := os.Stat(home); err != nil {
				t.Fatalf("the user's home is gone after Delete(%q): %v", name, err)
			}
			// The message itself, not only the directory: a root that exists
			// but was recreated is the failure this is about.
			if got := countMessages(t, root); got != 1 {
				t.Errorf("the mailbox holds %d messages after Delete(%q), want 1", got, name)
			}
		})
	}
}

// The same names are refused by every operation that writes, not only by
// Delete: Create would place cur/new/tmp over the mailbox root, and Save would
// deliver into it.
func TestDestructiveFolderNamesAreRefusedByEveryWrite(t *testing.T) {
	home := t.TempDir()
	u := openTestUser(t, home)

	for _, name := range []string{"", ".", ".."} {
		if err := u.Create(name); err == nil {
			t.Errorf("Create(%q) was accepted", name)
		}
		if _, _, _, err := u.Save(name, strings.NewReader("Subject: x\r\n\r\ny\r\n"),
			1, 20, nil, [16]byte{}); err == nil {
			t.Errorf("Save(%q) was accepted", name)
		}
		if err := u.Rename("Work", name); err == nil {
			t.Errorf("Rename to %q was accepted", name)
		}
		if err := u.Rename(name, "Work"); err == nil {
			t.Errorf("Rename from %q was accepted", name)
		}
	}
}

// And an ordinary name still works, or the tests above would pass on a backend
// that refuses everything.
func TestOrdinaryFolderNamesStillWork(t *testing.T) {
	home := t.TempDir()
	u := openTestUser(t, home)

	if err := u.Create("Work"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := u.Create("Work/Reports"); err != nil {
		t.Fatalf("Create nested: %v", err)
	}
	if _, _, _, err := u.Save("Work", strings.NewReader("Subject: x\r\n\r\ny\r\n"),
		1, 20, nil, [16]byte{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := u.Rename("Work/Reports", "Work/Old"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := u.Delete("Work/Old"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// INBOX names the root deliberately and must stay usable.
	if _, _, _, err := u.Save("INBOX", strings.NewReader("Subject: x\r\n\r\ny\r\n"),
		2, 20, nil, [16]byte{}); err != nil {
		t.Fatalf("Save to INBOX: %v", err)
	}
}

// countMessages counts what is in the mailbox, wherever maildir put it.
func countMessages(t *testing.T, root string) int {
	t.Helper()
	var n int
	for _, sub := range []string{"cur", "new"} {
		entries, err := os.ReadDir(filepath.Join(root, sub))
		if err != nil {
			continue
		}
		n += len(entries)
	}
	return n
}

func openTestUser(t *testing.T, home string) mailbox.UserMailbox {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@x", Home: home, Separator: "/"}
	u := New().OpenUser(info)
	t.Cleanup(func() { u.Close() }) //nolint:errcheck
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	return u
}

// Reading was the worse half. Destruction stays inside one account; a traversal
// on the read path crosses accounts, and it did — on a deployment whose IMAP
// separator is ".", `./../victim/Maildir` fetched another user's message.
//
// It failed with "/" only because the rewrite to the on-disk separator turned
// the traversal into a literal directory name. That is configuration, not a
// guarantee, which is why the check belongs where the path is built rather than
// on the operations that write.
func TestReadCannotEscapeTheAccount(t *testing.T) {
	for _, sep := range []string{"/", "."} {
		t.Run("separator="+sep, func(t *testing.T) {
			base := t.TempDir()
			victim := filepath.Join(base, "victim")
			attacker := filepath.Join(base, "attacker")
			if err := os.MkdirAll(filepath.Join(victim, "Maildir", "cur"), 0o700); err != nil {
				t.Fatal(err)
			}
			const secretName = "1234.M1P1.host:2,S"
			if err := os.WriteFile(filepath.Join(victim, "Maildir", "cur", secretName),
				[]byte("Subject: private\r\n\r\nvictim mail\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			info := &mailbox.UserInfo{Username: "attacker@x", Home: attacker, Separator: sep}
			u := New().OpenUser(info)
			t.Cleanup(func() { u.Close() }) //nolint:errcheck
			if err := u.Init(); err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{
				"./../victim/Maildir",
				"./../../victim/Maildir",
				"../victim/Maildir",
				"..",
				".",
			} {
				rc, err := u.Fetch(name, secretName, false)
				if err == nil {
					b, _ := io.ReadAll(rc)
					rc.Close() //nolint:errcheck
					t.Errorf("Fetch(%q) read %q from another account", name, string(b))
				}
			}
		})
	}
}
