package imap

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// healBox is a minimal UserMailbox whose only real method is the reactive heal.
// The embedded interface satisfies UserMailbox at compile time; any other method
// would panic if the test path called it (it does not).
type healBox struct {
	mailbox.UserMailbox
	calls    int
	expunged []uint32
	err      error
}

func (b *healBox) HealCorruptFolder(_ mailbox.UserIndex, _ *mailbox.Folder) ([]uint32, error) {
	b.calls++
	return b.expunged, b.err
}

// The session no longer bounds retries: that moved to the driver, where a
// reconnect does not reset it (#1682).
func TestTheSessionDoesNotBoundHealRetries(t *testing.T) {
	const folderID = uint64(7)
	box := &healBox{err: errors.New("scan incomplete")}
	s := &session{
		srv:           &Server{opts: Options{DboxReactiveRebuild: true}},
		markedCorrupt: map[uint64]bool{folderID: true},
	}
	h := &nsHandle{box: box}
	fsckd := &mailbox.Folder{ID: folderID, Name: "INBOX", Fsckd: true}

	for i := 0; i < 5; i++ {
		s.dboxHealIfCorrupt(h, "INBOX", fsckd)
	}
	if box.calls != 5 {
		t.Fatalf("the driver was called %d times, want 5: the session is bounding "+
			"retries again, and a reconnecting client resets that", box.calls)
	}
}

// A deferred heal is not a failure: the driver says it did not attempt.
func TestADeferredHealIsNotTreatedAsAFailure(t *testing.T) {
	const folderID = uint64(7)
	box := &healBox{err: mdbox.ErrHealDeferred}
	s := &session{
		srv:           &Server{opts: Options{DboxReactiveRebuild: true}},
		markedCorrupt: map[uint64]bool{folderID: true},
	}
	h := &nsHandle{box: box}
	fsckd := &mailbox.Folder{ID: folderID, Name: "INBOX", Fsckd: true}
	if got := s.dboxHealIfCorrupt(h, "INBOX", fsckd); got != nil {
		t.Error("a deferred heal returned a refreshed folder")
	}
	if !s.markedCorrupt[folderID] {
		t.Error("a deferred heal cleared the session's mark: the folder is still corrupt")
	}
}

// TestDboxHealStaleMarkClearing verifies that opening a folder which is no
// longer FSCKD (another session healed it) drops this session's stale per-session
// mark — but only when reactive rebuild is enabled. The mark is keyed by folder
// ID so the clear site matches the FETCH mark site regardless of folder name.
func TestDboxHealStaleMarkClearing(t *testing.T) {
	const folderID = uint64(7)
	cases := []struct {
		name        string
		enabled     bool
		wantCleared bool
	}{
		{"enabled clears stale mark on clean folder", true, true},
		{"disabled leaves mark untouched", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &session{
				srv:           &Server{opts: Options{DboxReactiveRebuild: c.enabled}},
				markedCorrupt: map[uint64]bool{folderID: true},
			}
			f := &mailbox.Folder{ID: folderID, Name: "INBOX", Fsckd: false}
			if got := s.dboxHealIfCorrupt(&nsHandle{}, "INBOX", f); got != nil {
				t.Fatalf("clean folder should return nil, got %v", got)
			}
			cleared := !s.markedCorrupt[folderID]
			if cleared != c.wantCleared {
				t.Errorf("mark cleared = %v, want %v", cleared, c.wantCleared)
			}
		})
	}
}
