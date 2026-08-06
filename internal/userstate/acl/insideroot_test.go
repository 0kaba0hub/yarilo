package acl

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The admin path writes the files the IMAP commands read, so a name IMAP
// refuses must not be writable here. "." and "/" were: on maildir both resolve
// to <mailroot>/../yarilo-acl — outside the namespace, in the directory every
// user's tree shares (#1091).
func TestStoreRefusesNamesThatLandOutsideTheNamespace(t *testing.T) {
	root := t.TempDir()
	s := New(root, root, "maildir", "/", "", "u@d.test", "test", Policy{}, nil)
	grant := mailbox.ACL{{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"}}

	// Only the names that actually escape. ".." is refused a layer up, by the
	// name rules, because on maildir it resolves to a directory called "..."
	// *inside* the root — junk, but not an escape, and this guard answers
	// about escapes.
	for _, folder := range []string{".", "/"} {
		err := s.Set(folder, grant)
		if err == nil {
			t.Errorf("Set(%q) was carried out; its ACL file lands outside %s", folder, root)
			continue
		}
		if !strings.Contains(err.Error(), "refusing folder") {
			t.Errorf("Set(%q) = %v; the refusal should name the folder it refused", folder, err)
		}
	}
}

// Ordinary names still work, or the guard would have disabled the command
// rather than bounded it.
func TestStoreStillWritesOrdinaryNames(t *testing.T) {
	root := t.TempDir()
	s := New(root, root, "maildir", "/", "", "u@d.test", "test", Policy{}, nil)
	grant := mailbox.ACL{{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}, Rights: "lr"}}

	for _, folder := range []string{"Sales", "Work/2026", "INBOX"} {
		if err := s.Set(folder, grant); err != nil {
			t.Errorf("Set(%q): %v", folder, err)
		}
	}
}

// The guard asks about the resolved path, not the name, so a namespace-root
// ACL file placed inside the root passes without the guard being taught its
// spelling. That is the property the bootstrap change (#1091 half two) needs
// from this one, and it is asserted here so the two compose rather than the
// second having to unpick the first.
func TestStoreAllowsAnyFileInsideTheNamespaceRoot(t *testing.T) {
	root := t.TempDir()
	s := New(root, root, "mdbox", "/", "", "u@d.test", "test", Policy{}, nil)

	// mdbox gives the root its own directory today, so "" is already such a
	// case: inside the root, not a folder anyone named.
	if err := s.checkInsideRoot(""); err != nil {
		t.Errorf("the namespace root was refused: %v", err)
	}
}

// The guard was written before the root ACL had a real spelling, so it asserted
// a property about a hypothesis: "a file inside the root will pass". Now that
// RootFileName exists, it is run against the name actually chosen, on every
// layout — the maildir case being the one the old collision made impossible.
func TestGuardAllowsTheRealNamespaceRootFile(t *testing.T) {
	for _, driver := range []string{"maildir", "mdbox", "sdbox"} {
		root := t.TempDir()
		s := New(root, root, driver, "/", "", "u@d.test", "test", Policy{}, nil)

		if err := s.checkInsideRoot(""); err != nil {
			t.Errorf("%s: the namespace root was refused by the guard: %v", driver, err)
		}
		// Not by assumption: the path the guard passed is the file the store
		// actually writes, and it is inside the tree the guard bounds.
		got := s.Path("")
		if filepath.Base(got) != RootFileName {
			t.Errorf("%s: Path(\"\") = %q, which is not the root ACL file", driver, got)
		}
		if !strings.HasPrefix(got, s.mailboxesRoot()) {
			t.Errorf("%s: root ACL %q is outside %q — the guard passed it for the wrong reason",
				driver, got, s.mailboxesRoot())
		}
	}
}
