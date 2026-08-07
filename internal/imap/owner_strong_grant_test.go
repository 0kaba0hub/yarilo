package imap_test

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// The strong owner grant (§3.7): the owner resolves to FullRights regardless of
// any entry, so a negative aimed at the owner -- the SETACL that would otherwise
// lock them out of their own mail -- changes nothing. Enforcement, MYRIGHTS and
// GETACL are checked together because the point of consolidating onto one
// mechanism is that the three agree by construction, not by coincidence: all
// three now read the owner from the resolver.
func TestOwnerStrongGrant_NegativeOnOwnerChangesNothing(t *testing.T) {
	aliceHome, dial := enforceServer(t)
	// Strip every right from alice on her own INBOX with a negative entry. It is
	// Negative, so GETACL still synthesises the implicit owner line rather than
	// showing this as her stored rights.
	seedACL(t, aliceHome, "INBOX", "-user=alice lrswipkxtea\n")

	c := dial("alice")

	// Enforcement: a create under INBOX needs 'k', which the negative strips --
	// the owner does it anyway.
	if err := c.Create("INBOX/Sub", nil).Wait(); err != nil {
		t.Errorf("owner CREATE despite a negative on herself: %v", err)
	}
	// Enforcement: SELECT needs 'r'/lookup, also stripped.
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Errorf("owner SELECT despite a negative on herself: %v", err)
	}

	// MYRIGHTS: full.
	mr, err := c.MyRights("INBOX").Wait()
	if err != nil {
		t.Fatalf("MYRIGHTS: %v", err)
	}
	if got := sortedString(string(mr.Rights)); got != sortedString(string(mailboxpkg.FullRights)) {
		t.Errorf("MYRIGHTS = %q, want full", got)
	}

	// GETACL: the owner line reads full, and it is the same answer MYRIGHTS gave
	// because both come from the resolver.
	ga, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatalf("GETACL: %v", err)
	}
	alice, _ := imaplib.NewRightsIdentifierUsername("alice")
	if got := sortedString(string(ga.Rights[alice])); got != sortedString(string(mailboxpkg.FullRights)) {
		t.Errorf("GETACL owner rights = %q, want full", got)
	}
}

// The owner never reaches the existence-hiding refusal. It is unreachable for
// the owner because they resolve to FullRights (no required right is ever
// absent), not because a helper returns early -- so if a resolver change ever
// let the owner miss the lookup right, this test fails instead of the symptom
// showing up as "the owner cannot see their own folder". Named in the comment
// on aclRefusal.
func TestOwnerNeverGetsHiddenExistence(t *testing.T) {
	aliceHome, dial := enforceServer(t)
	// Strip the lookup right from alice on her own INBOX. For a non-owner this
	// is exactly what turns a refusal into "No such mailbox" (#1068).
	seedACL(t, aliceHome, "INBOX", "-user=alice l\n")

	c := dial("alice")
	_, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		// A NONEXISTENT here would be the hiding path leaking to the owner.
		if aclErrCode(err) == imaplib.ResponseCodeNonExistent {
			t.Fatal("owner got NONEXISTENT on her own INBOX: the hiding path leaked to the owner")
		}
		t.Fatalf("owner SELECT of own INBOX: %v", err)
	}
}
