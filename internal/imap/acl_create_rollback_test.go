package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
)

// A CREATE that cannot grant its creator the admin right leaves a mailbox
// nobody can administer, and the state is sticky: the next CREATE of the same
// name fails with "already exists", so nothing self-heals, and repairing it
// needs the right that is missing (#1334).
//
// The window is real rather than theoretical -- CREATE is a local mkdir while
// the ACL write goes through yarilo-locks over the network -- so the failure is
// injected the same way it arrives: the ACL store refuses to write.
func TestCreateRollsBackWhenTheCreatorCannotBeGrantedAdmin(t *testing.T) {
	publicRoot, dial := publicNSServer(t)
	if err := os.WriteFile(filepath.Join(publicRoot, acl.RootFileName), []byte("user=alice lk\n"), 0o600); err != nil {
		t.Fatalf("seed root acl: %v", err)
	}

	a := dial("alice")

	// The ACL write fails: the directory a per-mailbox ACL would be written
	// into is replaced by something that cannot hold it. This stands in for the
	// operational case -- the lock service being unreachable -- without needing
	// one in the test.
	blockACLWrites(t, publicRoot, "Sales")

	err := a.Create("Public/Sales", nil).Wait()
	if err == nil {
		t.Fatal("CREATE reported success while the creator holds no admin right on the result")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "try again") {
		t.Errorf("the refusal does not tell the client to retry: %v", err)
	}

	// The mailbox must be gone: a client that saw NO and finds the name taken
	// can neither use it nor recreate it.
	if _, statErr := os.Stat(filepath.Join(publicRoot, ".Sales")); !os.IsNotExist(statErr) {
		t.Errorf("the mailbox survived a failed CREATE: %v", statErr)
	}
	// And the name is free again, which is the property that makes retrying
	// work once the ACL store answers.
	unblockACLWrites(t, publicRoot, "Sales")
	if err := a.Create("Public/Sales", nil).Wait(); err != nil {
		t.Errorf("the retry after the store recovered failed: %v", err)
	}
}

// The rollback must not fire when the grant works, which is every ordinary
// CREATE. Without this row a version that rolled back unconditionally would
// pass the one above and break the namespace entirely.
//
// A mailbox administered by inheritance never reaches the rollback either: the
// grant returns early when 'a' is already effective, which
// TestCreatorWithInheritedAdminIsLeftAlone pins. It is not re-tested here
// because there is no way to reach the rollback in that state -- the read that
// decides is the one inside the grant.
func TestCreateIsKeptWhenTheGrantSucceeds(t *testing.T) {
	publicRoot, dial := publicNSServer(t)
	if err := os.WriteFile(filepath.Join(publicRoot, acl.RootFileName), []byte("user=alice lk\n"), 0o600); err != nil {
		t.Fatalf("seed root acl: %v", err)
	}

	a := dial("alice")
	if err := a.Create("Public/Ops", nil).Wait(); err != nil {
		t.Fatalf("an ordinary CREATE was refused: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(publicRoot, ".Ops")); statErr != nil {
		t.Errorf("the mailbox is not there after a CREATE that reported success: %v", statErr)
	}
	if err := a.SetACL("Public/Ops", "user=bob", imap.RightModificationReplace, imap.RightSet("lr")).Wait(); err != nil {
		t.Errorf("the creator cannot administer the mailbox it created: %v", err)
	}
}

// blockACLWrites makes the per-mailbox ACL file unwritable by putting a
// directory where the file has to go. The mailbox itself is created by the
// server a moment later, in the same place, which is what makes this a
// half-failure rather than a failed CREATE.
func blockACLWrites(t *testing.T, publicRoot, folder string) {
	t.Helper()
	dir := filepath.Join(publicRoot, "."+folder)
	if err := os.MkdirAll(filepath.Join(dir, "yarilo-acl"), 0o700); err != nil {
		t.Fatalf("block acl writes: %v", err)
	}
}

func unblockACLWrites(t *testing.T, publicRoot, folder string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(publicRoot, "."+folder)); err != nil {
		t.Fatalf("unblock acl writes: %v", err)
	}
}
