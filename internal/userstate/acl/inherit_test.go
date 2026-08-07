package acl

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func userEntry(name, rights string) mailbox.Entry {
	return mailbox.Entry{Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: name}, Rights: mailbox.Rights(rights)}
}

// Inheritance is materialised once, at creation: the new mailbox's own file
// carries what it inherited, and answers "who has rights here" by itself from
// then on. Resolving it live instead is what made the first per-mailbox grant
// revoke the granter -- they held their rights through the root and the new
// file named only the peer (#1111).
func TestMaterialiseOnCreateWritesWhatTheMailboxInherits(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
	if err := s.Set("", mailbox.ACL{userEntry("bob", "lrskxa")}); err != nil {
		t.Fatalf("Set root: %v", err)
	}

	if err := s.MaterialiseOnCreate("Matrix"); err != nil {
		t.Fatalf("MaterialiseOnCreate: %v", err)
	}
	got, err := s.Get("Matrix")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 1 || got[0].Identifier.Name != "bob" || string(got[0].Rights) != "lrskxa" {
		t.Fatalf("new mailbox ACL = %+v, want bob's root grant copied in", got)
	}

	// A mailbox that already has one is left alone: materialising twice must
	// not re-open a decision the file already records.
	if err := s.Set("Matrix", mailbox.ACL{userEntry("carol", "l")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.MaterialiseOnCreate("Matrix"); err != nil {
		t.Fatalf("MaterialiseOnCreate again: %v", err)
	}
	got, _ = s.Get("Matrix")
	if len(got) != 1 || got[0].Identifier.Name != "carol" {
		t.Errorf("existing ACL was rewritten: %+v", got)
	}

	// A child inherits from the nearest ancestor with an ACL, not from the root.
	if err := s.MaterialiseOnCreate("Matrix/Sub"); err != nil {
		t.Fatalf("MaterialiseOnCreate child: %v", err)
	}
	got, _ = s.Get("Matrix/Sub")
	if len(got) != 1 || got[0].Identifier.Name != "carol" {
		t.Errorf("child ACL = %+v, want the parent's entry", got)
	}
}

// The repair for mailboxes created before copy-at-create: add what they
// inherit and do not already name. Never rewrite what is there -- an existing
// entry is an explicit statement, and "orphaned by the old rule" and "written
// to leave that identifier out" are the same file on disk.
func TestMaterialiseExistingAddsOnlyWhatIsMissing(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
	if err := s.Set("", mailbox.ACL{userEntry("bob", "lrskxa"), userEntry("dave", "lr")}); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	// The state #1111 leaves behind, plus one identifier the mailbox names
	// with different rights on purpose.
	if err := s.Set("Matrix", mailbox.ACL{userEntry("carol", "l"), userEntry("dave", "l")}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Dry run first: it must report and change nothing.
	rep, err := s.MaterialiseExisting([]string{"Matrix"}, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// The rights are the point of the report: two identifiers with no rights
	// beside them print a repair and a widening identically.
	if got := rep.Added["Matrix"]; len(got) != 1 || got[0].Identifier != "user=bob" || got[0].Rights != "lrskxa" {
		t.Errorf("dry run added = %+v, want user=bob with the rights it would gain", got)
	}
	if got := rep.Skipped["Matrix"]; len(got) != 1 || got[0].Identifier != "user=dave" || got[0].Rights != "l" {
		t.Errorf("dry run skipped = %+v, want user=dave with the rights the mailbox keeps giving them", got)
	}
	after, _ := s.Get("Matrix")
	if len(after) != 2 {
		t.Fatalf("the dry run wrote to disk: %+v", after)
	}

	// Apply.
	if _, err := s.MaterialiseExisting([]string{"Matrix"}, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after, _ = s.Get("Matrix")
	if len(after) != 3 {
		t.Fatalf("after apply = %+v, want three entries", after)
	}
	for _, e := range after {
		if e.Identifier.Name == "dave" && string(e.Rights) != "l" {
			t.Errorf("dave's own entry was rewritten to %q; the mailbox's statement wins", e.Rights)
		}
	}

	// Idempotent: the second run adds nothing.
	rep2, err := s.MaterialiseExisting([]string{"Matrix"}, false)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(rep2.Added) != 0 {
		t.Errorf("second run added %v", rep2.Added)
	}
	again, _ := s.Get("Matrix")
	if len(again) != 3 {
		t.Errorf("second run changed the file: %+v", again)
	}
}

// A mailbox with no ACL of its own is left without one: it already resolves to
// what it inherits, and writing a file would freeze a value that is still live.
func TestMaterialiseExistingLeavesInheritingMailboxesAlone(t *testing.T) {
	home := t.TempDir()
	s := New(home, "", "mdbox", "/", "", "alice", "test", Policy{}, nil)
	if err := s.Set("", mailbox.ACL{userEntry("bob", "lr")}); err != nil {
		t.Fatalf("Set root: %v", err)
	}
	rep, err := s.MaterialiseExisting([]string{"Plain"}, false)
	if err != nil {
		t.Fatalf("MaterialiseExisting: %v", err)
	}
	if len(rep.Added) != 0 {
		t.Errorf("added %v to a mailbox that has no ACL of its own", rep.Added)
	}
	if got, _ := s.Get("Plain"); got != nil {
		t.Errorf("a file was created for an inheriting mailbox: %+v", got)
	}
}
