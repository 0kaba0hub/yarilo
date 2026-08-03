package jmap

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Cross-protocol identity must be a construction, not a coincidence: an id a
// client reads over JMAP has to be the string IMAP reports for the same object
// (RFC 8474 EMAILID / MAILBOXID). Formatting it inline here would keep working
// until the day the shared formatter changes, and then diverge silently — this
// test is the anchor that stops the inline version coming back.
func TestObjectIDsUseTheSharedFormatter(t *testing.T) {
	guids := [][16]byte{
		{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		{0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff},
		{}, // all-zero: still has to agree, however meaningless the value
	}
	for _, guid := range guids {
		want := mailbox.FormatObjectID(guid)

		if got := emailID(&mailbox.MessageMeta{GUID: guid}); got != want {
			t.Errorf("emailID = %q, IMAP EMAILID = %q", got, want)
		}
		// mailboxID is what mailboxList actually calls, so the anchor pins the
		// production path rather than restating the formatter.
		if got := mailboxID(guid); got != want {
			t.Errorf("mailbox id = %q, IMAP MAILBOXID = %q", got, want)
		}
	}
}

// The state string is a digest of the mailbox set, not an object id, so it must
// NOT be routed through the identity formatter — the two answer different
// questions and tying them together would couple a cache key to an id format.
func TestMailboxStateIsNotAnObjectID(t *testing.T) {
	state := mailboxState(nil)
	if len(state) != 16 {
		t.Errorf("state is %d chars; an object id would be 32", len(state))
	}
}
