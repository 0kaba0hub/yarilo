package imap

import (
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestDboxHealClearsStaleMarkOnCleanFolder verifies that opening a folder which
// is no longer FSCKD (another session healed it) drops this session's stale
// per-session mark, so a fresh corruption re-flags the folder.
func TestDboxHealClearsStaleMarkOnCleanFolder(t *testing.T) {
	s := &session{
		srv:           &Server{opts: Options{DboxReactiveRebuild: true}},
		markedCorrupt: map[string]bool{"INBOX": true},
	}
	if got := s.dboxHealIfCorrupt(&nsHandle{}, "INBOX", &mailbox.Folder{Name: "INBOX", Fsckd: false}); got != nil {
		t.Fatalf("clean folder should return nil, got %v", got)
	}
	if s.markedCorrupt["INBOX"] {
		t.Error("stale per-session mark must be cleared when the folder is no longer Fsckd")
	}
}

// TestDboxHealDisabledIsNoop verifies the whole path is inert when reactive
// rebuild is disabled — including the stale-mark clearing.
func TestDboxHealDisabledIsNoop(t *testing.T) {
	s := &session{
		srv:           &Server{opts: Options{DboxReactiveRebuild: false}},
		markedCorrupt: map[string]bool{"INBOX": true},
	}
	if got := s.dboxHealIfCorrupt(&nsHandle{}, "INBOX", &mailbox.Folder{Name: "INBOX", Fsckd: false}); got != nil {
		t.Fatalf("disabled should return nil, got %v", got)
	}
	if !s.markedCorrupt["INBOX"] {
		t.Error("disabled: the mark must be left untouched")
	}
}
