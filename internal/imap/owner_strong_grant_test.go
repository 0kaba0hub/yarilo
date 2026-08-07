package imap_test

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	mailboxpkg "github.com/yarilomail/yarilo/pkg/mailbox"
)

// The strong owner grant (§7.6): the owner resolves to FullRights regardless of
// any stored entry, so an entry that names the owner -- a reduced user=<owner>,
// a -user=<owner> negative, or the inert owner keyword -- changes nothing.
//
// Enforcement, MYRIGHTS and GETACL are checked together: the point of one
// mechanism is that the three agree by construction. GETACL must not surface a
// second row for the owner that contradicts what MYRIGHTS resolved -- the seeds
// below each produced exactly such a row before this fix (a reduced positive
// read as the owner's rights, a negative under the -alice key, the owner keyword
// as its own row), which the fixture distinguishes by asserting the absent keys,
// not only ga.Rights[alice].
func TestOwnerStrongGrant_OwnerNamingEntriesAreInert(t *testing.T) {
	full := sortedString(string(mailboxpkg.FullRights))
	alicePos, _ := imaplib.NewRightsIdentifierUsername("alice")
	aliceNeg := imaplib.RightsIdentifier("-alice")
	ownerKw := imaplib.RightsIdentifier("owner")

	seeds := []struct {
		name string
		acl  string
		// the owner-naming key that must NOT appear as a separate GETACL row.
		absentKey imaplib.RightsIdentifier
	}{
		{"reduced positive user= for the owner", "user=alice lr\n", ""},
		{"negative on the owner", "-user=alice lrswipkxtea\n", aliceNeg},
		{"inert owner keyword", "owner lr\n", ownerKw},
	}

	for _, tc := range seeds {
		t.Run(tc.name, func(t *testing.T) {
			aliceHome, dial := enforceServer(t)
			seedACL(t, aliceHome, "INBOX", tc.acl)
			c := dial("alice")

			// Enforcement: a create under INBOX needs 'k' and SELECT needs
			// 'r'/lookup -- both are what the reduced/negative seeds withhold, and
			// the owner does them anyway.
			if err := c.Create("INBOX/Sub", nil).Wait(); err != nil {
				t.Errorf("owner CREATE despite %q: %v", tc.acl, err)
			}
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Errorf("owner SELECT despite %q: %v", tc.acl, err)
			}

			mr, err := c.MyRights("INBOX").Wait()
			if err != nil {
				t.Fatalf("MYRIGHTS: %v", err)
			}
			if got := sortedString(string(mr.Rights)); got != full {
				t.Errorf("MYRIGHTS = %q, want full", got)
			}

			ga, err := c.GetACL("INBOX").Wait()
			if err != nil {
				t.Fatalf("GETACL: %v", err)
			}
			// The single owner row equals MYRIGHTS.
			if got := sortedString(string(ga.Rights[alicePos])); got != full {
				t.Errorf("GETACL owner row = %q, want full (must match MYRIGHTS)", got)
			}
			// And there is no contradicting owner-naming row beside it.
			if tc.absentKey != "" {
				if _, ok := ga.Rights[tc.absentKey]; ok {
					t.Errorf("GETACL surfaced an inert owner-naming row %q: %v", tc.absentKey, ga.Rights)
				}
			}
		})
	}
}

// SETACL that names the owner is refused, not answered OK over a no-op: the
// owner cannot be capped or granted through the ACL (strong grant), so a write
// that names them would change nothing while replying OK -- the shape #1114
// removed. The operator learns at the write that freezing is a separate
// mechanism.
func TestOwnerStrongGrant_SetACLOnOwnerRefused(t *testing.T) {
	aliceHome, dial := enforceServer(t)
	_ = aliceHome
	c := dial("alice")

	// A reduced positive, a negative, and the owner keyword are all refused.
	alice, _ := imaplib.NewRightsIdentifierUsername("alice")
	for _, id := range []imaplib.RightsIdentifier{alice, imaplib.RightsIdentifier("-alice"), imaplib.RightsIdentifier("owner")} {
		if err := c.SetACL("INBOX", id, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait(); err == nil {
			t.Errorf("SETACL naming the owner (%q) answered OK; want a refusal", id)
		}
	}
	// DELETEACL naming the owner is refused too.
	if err := c.DeleteACL("INBOX", alice).Wait(); err == nil {
		t.Error("DELETEACL naming the owner answered OK; want a refusal")
	}

	// A non-owner identifier is unaffected: alice can still administer peers.
	bob, _ := imaplib.NewRightsIdentifierUsername("bob")
	if err := c.SetACL("INBOX", bob, imaplib.RightModificationReplace, imaplib.RightSet("lr")).Wait(); err != nil {
		t.Errorf("SETACL for a peer must still work: %v", err)
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
