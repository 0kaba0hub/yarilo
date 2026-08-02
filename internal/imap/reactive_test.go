package imap

import (
	"errors"
	"testing"

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

// TestDboxHealRetryBound: when the heal keeps failing (a purge/altmove keeps the
// scan incomplete), the session stops auto-retrying the folder after
// maxHealAttempts, and a later clean open (marker cleared elsewhere) resets the
// counter so a fresh corruption is healed again.
func TestDboxHealRetryBound(t *testing.T) {
	const folderID = uint64(7)
	box := &healBox{err: errors.New("scan incomplete")}
	s := &session{
		srv:           &Server{opts: Options{DboxReactiveRebuild: true}},
		markedCorrupt: map[uint64]bool{folderID: true},
	}
	h := &nsHandle{box: box}
	fsckd := &mailbox.Folder{ID: folderID, Name: "INBOX", Fsckd: true}

	// Many opens over a persistently-failing folder call the heal at most
	// maxHealAttempts times, then stop.
	for i := 0; i < maxHealAttempts+5; i++ {
		s.dboxHealIfCorrupt(h, "INBOX", fsckd)
	}
	if box.calls != maxHealAttempts {
		t.Fatalf("heal attempts = %d, want capped at %d", box.calls, maxHealAttempts)
	}

	// The marker clearing elsewhere (folder now clean) resets the counter.
	clean := &mailbox.Folder{ID: folderID, Name: "INBOX", Fsckd: false}
	s.dboxHealIfCorrupt(h, "INBOX", clean)
	if s.healAttempts[folderID] != 0 {
		t.Fatalf("counter = %d after clean open, want reset to 0", s.healAttempts[folderID])
	}

	// A fresh corruption is retried again from zero.
	s.dboxHealIfCorrupt(h, "INBOX", fsckd)
	if box.calls != maxHealAttempts+1 {
		t.Fatalf("heal attempts = %d after reset, want %d", box.calls, maxHealAttempts+1)
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
