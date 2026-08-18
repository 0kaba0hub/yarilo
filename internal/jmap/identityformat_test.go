package jmap

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
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

// The state string describes the mailbox set; it is not an object id, and must
// not be routed through the identity formatter -- the two answer different
// questions, and tying them together would couple a client's cache key to an id
// format. It carries its own version instead, which is what lets a later
// consumer refuse a layout it does not know rather than misread it.
func TestMailboxStateIsNotAnObjectID(t *testing.T) {
	state := mailboxState(nil)
	if len(state) == 32 {
		t.Errorf("state %q has an object id's shape", state)
	}
	if !strings.HasPrefix(state, "1-") {
		t.Errorf("state %q carries no format version", state)
	}
	if _, err := jmapcore.ParseDescription(state, jmapcore.KindMailbox); err != nil {
		t.Errorf("the state we emit does not parse as one of ours: %v", err)
	}
}
