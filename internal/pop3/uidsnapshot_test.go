package pop3

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// batchLocker is enough of a lock service for expungeDeleted to take its
// batch path. Embedded nil, so a call it does not implement panics rather than
// being answered with a zero value.
type batchLocker struct {
	locks.Locker
}

func (batchLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	return locks.Lock{ID: fmt.Sprintf("%s/%s", resource, owner), Resource: resource, Owner: owner}, nil
}
func (batchLocker) Unlock(_ context.Context, _ string) error { return nil }
func (batchLocker) HoldsResource(_ string) bool              { return false }
func (batchLocker) Emit(_ context.Context, _ string, _ locks.EventType, _ string) error {
	return nil
}

// POP3 reads its maildrop once, at login, and answers every later command from
// that snapshot. What makes the read safe to serve without the cross-process
// lock is not the read but what deletion addresses: a UID taken from the
// snapshot, never a position in a freshly read index. A snapshot one delivery
// behind then narrows the session's view — legal under RFC 1939, which requires
// a constant maildrop for the session — instead of misdirecting a DELE onto a
// message the client never saw.
//
// Asserted through the real expungeDeleted, not a copy of its loop: a rewrite
// that started addressing by position has to fail this row, and it only can if
// the row runs the code under test.
func TestDeletionAddressesUIDsFromTheSnapshot(t *testing.T) {
	// Both paths, because there are two: with a lock service the whole batch
	// runs under one lock, without one it is per message. A row covering one
	// leaves the other free to start addressing by position.
	for _, tc := range []struct {
		name   string
		locker locks.Locker
	}{
		{"batched under one lock", batchLocker{}},
		{"per message, no lock service", nil},
	} {
		t.Run(tc.name, func(t *testing.T) { assertDeletionUsesSnapshotUIDs(t, tc.locker) })
	}
}

func assertDeletionUsesSnapshotUIDs(t *testing.T, locker locks.Locker) {
	t.Helper()
	idx := &mockIndex{}
	box := &mockMailbox{}
	s := &session{
		srv:      &Server{opts: Options{Locker: locker}},
		idx:      idx,
		box:      box,
		userInfo: &mailbox.UserInfo{Username: "u@example.org"},
		folder:   &mailbox.Folder{ID: 1, Name: "INBOX"},
		msgs: []*mailbox.MessageMeta{
			{UID: 11, Filename: "a"},
			{UID: 12, Filename: "b"},
			{UID: 13, Filename: "c"},
		},
	}
	s.deleted = make([]bool, len(s.msgs))

	// The client deletes message number 2 of its session view.
	s.deleted[1] = true

	if errs := s.expungeDeleted(); errs != 0 {
		t.Fatalf("expungeDeleted reported %d errors", errs)
	}

	if !slices.Equal(idx.expunged, []uint32{12}) {
		t.Fatalf("expunged %v, want the UID from the snapshot ([12])", idx.expunged)
	}

	// The fixture has to be one where the two addressings disagree, or the row
	// proves nothing: with a newer message at the front, position 2 names a
	// different UID than the snapshot does.
	fresher := []*mailbox.MessageMeta{
		{UID: 10, Filename: "new"},
		{UID: 11, Filename: "a"},
		{UID: 12, Filename: "b"},
	}
	if fresher[1].UID == idx.expunged[0] {
		t.Error("position addressing and UID addressing agree on this fixture, so it distinguishes nothing")
	}
}
