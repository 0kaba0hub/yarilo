package imap

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

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
