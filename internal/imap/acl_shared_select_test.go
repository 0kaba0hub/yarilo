package imap_test

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// #1317, reproduced: a peer with no ACL entry SELECTs a mailbox another user
// created in the shared namespace, and gets it read-write. The same surface
// answers differently for a name that does not exist, so it enumerates too.
func TestPeerCannotSelectAMailboxItHasNoRightsOn(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)

	a := dial("alice")
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("alice SELECT INBOX: %v", err)
	}
	// Alice needs the create right on the namespace root, exactly as the
	// sandbox grant did -- this is setup, not the subject.
	seedRootACL(t, aliceHome, "user=alice lk\n")

	if err := a.Create("Shared/Probe", nil).Wait(); err != nil {
		t.Fatalf("alice CREATE: %v", err)
	}

	b := dial("bob") // no entry anywhere, on this mailbox or above it

	_, selErr := b.Select("Shared/Probe", nil).Wait()
	if selErr == nil {
		t.Error("a peer with no rights SELECTed the mailbox")
	}
	_, absentErr := b.Select("Shared/Absent", nil).Wait()
	if absentErr == nil {
		t.Fatal("SELECT of a name that does not exist succeeded")
	}

	// The oracle: whatever the refusal is, it must be the same one, or the
	// peer learns which names are there by comparing them.
	if selErr != nil && aclErrCode(selErr) != aclErrCode(absentErr) {
		t.Errorf("existing mailbox answers %q, absent one %q -- the difference enumerates names",
			aclErrCode(selErr), aclErrCode(absentErr))
	}
	if code := aclErrCode(absentErr); code != imap.ResponseCodeNonExistent {
		t.Errorf("absent mailbox answered %q, want NONEXISTENT", code)
	}
}
