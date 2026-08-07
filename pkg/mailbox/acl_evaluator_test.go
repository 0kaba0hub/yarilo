package mailbox_test

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The three evaluator defects of #1117, each with an input that tells the two
// behaviours apart. That is the point of this file: every earlier version of
// these assertions passed under the defect because the fixture could not
// distinguish "replace" from "merge", or "skip" from "evaluate".
//
// Each case says what the old code answered, so a future reader can see the
// input was chosen to separate them rather than to illustrate the fix.
func TestEvaluatorMatrix(t *testing.T) {
	parse := func(t *testing.T, body string) mailbox.ACL {
		t.Helper()
		acl, err := mailbox.ParseACLString(body)
		if err != nil {
			t.Fatalf("parse %q: %v", body, err)
		}
		return acl
	}

	cases := []struct {
		name    string
		local   string
		global  string
		user    string
		groups  []string
		isOwner bool
		want    string
		wasa    string // what the code answered before #1117
	}{
		// --- negatives: one tier boundary per sign -------------------------
		{
			// Needs the lower-tier negative: without it, replacing and merging
			// the user-tier negative give the same answer.
			name:  "user-tier negative replaces the authenticated-tier one",
			local: "-authenticated r\nuser=alice lrs\n-user=alice s\n",
			user:  "alice", want: "lr", wasa: "l",
		},
		{
			name:  "and the file order of the two signs does not decide it",
			local: "-authenticated r\n-user=alice s\nuser=alice lrs\n",
			user:  "alice", want: "lr", wasa: "lr",
		},
		// --- owner: the strong grant beats every entry ---------------------
		// The owner resolves to FullRights unconditionally (§3.7). No entry --
		// below the owner tier, above it, or an explicit owner entry that grants
		// less -- reduces the owner, because a shared/personal namespace has no
		// second owner to repair one SETACL that locked the first out.
		{
			name:  "a negative below the owner tier does not touch the owner",
			local: "-anyone a\n",
			user:  "alice", isOwner: true, want: "lrswipkxtea",
		},
		{
			// group-override sits above the owner tier, yet the strong grant
			// wins: the owner is not reducible through the ACL at all.
			name:  "group-override does not restrict the owner",
			local: "group-override=locked lr\n",
			user:  "alice", groups: []string{"locked"}, isOwner: true,
			want: "lrswipkxtea",
		},
		{
			// A user= negative aimed at the owner is exactly the SETACL that
			// would otherwise lock the owner out of their own mail.
			name:  "a user negative on the owner does not reduce the owner",
			local: "user=alice lrswipkxtea\n-user=alice a\n",
			user:  "alice", isOwner: true, want: "lrswipkxtea",
		},
		{
			// An explicit owner entry that grants less is overridden too: the
			// owner cannot be capped below full.
			name:  "an explicit reduced owner entry does not cap the owner",
			local: "owner lr\n",
			user:  "alice", isOwner: true, want: "lrswipkxtea",
		},
		// --- global precedence ---------------------------------------------
		{
			// The fail-open one: any matching global used to discard every
			// local negative, so an unrelated global grant re-granted 'a'.
			name:   "an unrelated global does not re-grant a locally revoked right",
			local:  "user=alice lra\n-user=alice a\n",
			global: "anyone l",
			user:   "alice", want: "l", wasa: "alr",
		},
		{
			// A global that speaks only about the negative mask leaves the
			// local positives alone — otherwise a revoke would blank the ACL.
			name:   "a global negative alone does not blank the local grant",
			local:  "user=bob lrs\n",
			global: "-anyone s",
			user:   "bob", want: "lr", wasa: "lr",
		},
		{
			name:   "a global positive replaces the local grant",
			local:  "user=bob lr\n",
			global: "anyone i",
			user:   "bob", want: "i", wasa: "ilr",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var global mailbox.ACL
			if tc.global != "" {
				global = parse(t, tc.global)
			}
			got := mailbox.EffectiveWithGlobal(parse(t, tc.local), global, tc.user, tc.groups, tc.isOwner)
			if string(got) != tc.want {
				t.Errorf("effective = %q, want %q (before #1117 it answered %q)", got, tc.want, tc.wasa)
			}
		})
	}
}
