package imap

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// isOwner is by person, not by namespace type. The distinguishing cases are the
// two where the two definitions disagree: a non-personal handle whose owner is
// the session user (person says owned, type says not), and a personal-type
// handle whose owner is someone else (person says not, type says owned). A test
// that only checked "personal → owned, shared → not" would pass under the old
// type predicate too and prove nothing (#1107; the two-rules section in
// ARCHITECTURE.md).
func TestIsOwnerIsByPersonNotType(t *testing.T) {
	sess := func(user string) *session {
		return &session{userInfo: &mailbox.UserInfo{Username: user}}
	}
	cases := []struct {
		name          string
		user          string
		hType         NamespaceType
		hOwner        string
		want          bool
		distinguishes string
	}{
		{"personal owned by the session user", "alice", NamespacePersonal, "alice", true, "agrees with type"},
		{"fixed shared owned by nobody", "alice", NamespaceShared, "", false, "agrees with type"},
		{"other-users owned by nobody", "alice", NamespaceOther, "", false, "agrees with type"},
		// The two that separate person from type:
		{"owner-templated instance owned by the session user", "alice", NamespaceShared, "alice", true,
			"type would say not owned"},
		{"personal-type handle owned by someone else", "alice", NamespacePersonal, "bob", false,
			"type would say owned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &nsHandle{spec: NamespaceSpec{Type: tc.hType}, owner: tc.hOwner}
			if got := sess(tc.user).isOwner(h); got != tc.want {
				t.Errorf("isOwner = %v, want %v (%s)", got, tc.want, tc.distinguishes)
			}
		})
	}
}
