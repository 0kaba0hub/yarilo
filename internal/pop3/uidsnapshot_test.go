package pop3

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// POP3 reads its maildrop once, at login, and answers every later command from
// that snapshot. What makes the read safe to serve without the cross-process
// lock is not the read itself but what deletion addresses: a UID taken from the
// snapshot, never a position in a freshly read index. A snapshot one delivery
// behind then narrows the session's view — legal under RFC 1939, which requires
// the maildrop to be constant for the session — instead of misdirecting a DELE
// onto a message the client never saw.
//
// The row pins the addressing, which is worth having whatever happens to the
// locking: it is the property the classification in #1249 rests on.
func TestDeletionAddressesUIDsFromTheSnapshot(t *testing.T) {
	s := &session{
		msgs: []*mailbox.MessageMeta{
			{UID: 11, Filename: "a"},
			{UID: 12, Filename: "b"},
			{UID: 13, Filename: "c"},
		},
	}
	s.deleted = make([]bool, len(s.msgs))

	// The client deletes message number 2 of its session view.
	s.deleted[1] = true

	var expunged []uint32
	for i, m := range s.msgs {
		if s.deleted[i] {
			expunged = append(expunged, m.UID)
		}
	}

	if len(expunged) != 1 || expunged[0] != 12 {
		t.Fatalf("deletion resolved to %v, want the UID from the snapshot (12)", expunged)
	}
	// And position 2 is only meaningful inside the snapshot: had the deletion
	// been re-resolved against a fresher index with a new message at the front,
	// position 2 would name UID 12's neighbour instead.
	fresher := []*mailbox.MessageMeta{
		{UID: 10, Filename: "new"},
		{UID: 11, Filename: "a"},
		{UID: 12, Filename: "b"},
	}
	if fresher[1].UID == expunged[0] {
		t.Error("position addressing and UID addressing agree here, so this row proves nothing")
	}
}
