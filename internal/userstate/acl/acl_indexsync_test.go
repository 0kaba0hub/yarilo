package acl

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Update -- the read-modify-write IMAP SETACL/DELETEACL use -- must keep the
// yarilo-acl-list index in step with the per-mailbox file. Before #1147 it
// wrote the file and left the index untouched, so `yarctl acl list` reported
// grants that were changed or removed while `yarctl acl get` (the file) was
// right.
func TestUpdate_KeepsListIndexInSync(t *testing.T) {
	s := New(t.TempDir(), "", "", "/", "", "alice", "test", Policy{}, nil)
	bob := mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}

	// SETACL-style: grant bob lr.
	mustUpdate(t, s, "IdxGate", func(cur mailbox.ACL) mailbox.ACL {
		return append(cur, mailbox.Entry{Identifier: bob, Rights: "lr"})
	})
	if !listRights(t, s, "IdxGate", bob).Has('r') {
		t.Fatal("index does not reflect the grant Update wrote (#1147)")
	}

	// SETACL-style: change it to lrs -- the index must follow, not keep lr.
	mustUpdate(t, s, "IdxGate", func(mailbox.ACL) mailbox.ACL {
		return mailbox.ACL{{Identifier: bob, Rights: "lrs"}}
	})
	if got := listRights(t, s, "IdxGate", bob); string(got) != "lrs" {
		t.Errorf("index rights = %q, want lrs (index kept the old value)", got)
	}

	// A no-change Update (nil) leaves both alone.
	mustUpdate(t, s, "IdxGate", func(mailbox.ACL) mailbox.ACL { return nil })
	if got := listRights(t, s, "IdxGate", bob); string(got) != "lrs" {
		t.Errorf("nil Update changed the index to %q", got)
	}

	// DELETEACL-style: remove the last entry -> empty (non-nil) ACL. The stale
	// row must leave the index, not linger.
	mustUpdate(t, s, "IdxGate", func(mailbox.ACL) mailbox.ACL { return mailbox.ACL{} })
	if listHasFolder(t, s, "IdxGate") {
		t.Error("index still lists a grant after removal (#1147 drift)")
	}
	if got, _ := s.Get("IdxGate"); len(got) != 0 {
		t.Errorf("file still holds entries after removal: %v", got)
	}
}

func mustUpdate(t *testing.T, s *Store, folder string, f func(mailbox.ACL) mailbox.ACL) {
	t.Helper()
	if err := s.Update(folder, func(cur mailbox.ACL) (mailbox.ACL, error) { return f(cur), nil }); err != nil {
		t.Fatalf("Update %s: %v", folder, err)
	}
}

func listRights(t *testing.T, s *Store, folder string, id mailbox.Identifier) mailbox.Rights {
	t.Helper()
	snap, err := s.ListSnapshot()
	if err != nil {
		t.Fatalf("ListSnapshot: %v", err)
	}
	for _, e := range snap {
		if e.Mailbox == folder && e.Identifier == id && !e.Negative {
			return e.Rights
		}
	}
	return ""
}

func listHasFolder(t *testing.T, s *Store, folder string) bool {
	t.Helper()
	snap, err := s.ListSnapshot()
	if err != nil {
		t.Fatalf("ListSnapshot: %v", err)
	}
	for _, e := range snap {
		if e.Mailbox == folder {
			return true
		}
	}
	return false
}
