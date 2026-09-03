package mailbox

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// A namespace handle takes the same lock owner as the session it came from.
//
// Asserted through locks.Owner rather than by comparing the field, because the
// field is only interesting for what it makes: a namespace UserInfo that dropped
// the session reached the lock service under a second spelling of one holder,
// which is 386 of 8,503 refusal lines in a measured window (#1652).
//
// The end-to-end owner test in the imap package cannot reach this. A session id
// arrives from the login proxy's preamble, so an ordinary test connection has
// none and every path there produces the sessionless form -- the same limit that
// sent the session's own coverage into pkg/locks.
func TestANamespaceHandleKeepsTheSessionInItsLockOwner(t *testing.T) {
	base := &UserInfo{
		Username:  "u56@example.com",
		SessionID: "4hZsLQ3CbjzBBcd1f9",
		Home:      "/srv/mail/u56",
	}
	ns, err := NamespaceUserInfo(base, Location{Path: "/srv/public", Driver: "maildir"}, "/")
	if err != nil {
		t.Fatalf("NamespaceUserInfo: %v", err)
	}
	want := locks.Owner(base.Username, base.SessionID)
	if got := locks.Owner(ns.Username, ns.SessionID); got != want {
		t.Errorf("a namespace handle locks as %q where its session locks as %q", got, want)
	}
}
