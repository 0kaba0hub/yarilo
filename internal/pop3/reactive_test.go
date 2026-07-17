package pop3

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// fakeCorruptBox always fails Fetch with ErrCorruptStorage and can heal, so it
// passes the CanReactiveHeal gate. Other UserMailbox methods are unused (the
// embedded nil interface panics only if called, which the test never does).
type fakeCorruptBox struct{ mailbox.UserMailbox }

func (fakeCorruptBox) Fetch(string, string, bool) (io.ReadCloser, error) {
	return nil, fmt.Errorf("file gone: %w", mailbox.ErrCorruptStorage)
}
func (fakeCorruptBox) HealCorruptFolder(mailbox.UserIndex, *mailbox.Folder) (int, error) {
	return 0, nil
}

// fakeMarkIdx counts MarkFolderCorrupt calls; OpenFolder returns a stub folder.
type fakeMarkIdx struct {
	mailbox.UserIndex
	marks int
}

func (f *fakeMarkIdx) OpenFolder(string, uint32) (*mailbox.Folder, error) {
	return &mailbox.Folder{ID: 1, Name: "INBOX"}, nil
}
func (f *fakeMarkIdx) MarkFolderCorrupt(uint64) error  { f.marks++; return nil }
func (f *fakeMarkIdx) ClearFolderCorrupt(uint64) error { return nil }

// TestFetchINBOXGatesMarking verifies a RETR loop over a corrupt mailbox marks
// the folder FSCKD at most once per session, not once per message.
func TestFetchINBOXGatesMarking(t *testing.T) {
	idx := &fakeMarkIdx{}
	s := &session{box: fakeCorruptBox{}, idx: idx}

	for i := 0; i < 5; i++ {
		_, err := s.fetchINBOX(&mailbox.MessageMeta{Filename: "1"})
		if !errors.Is(err, mailbox.ErrCorruptStorage) {
			t.Fatalf("fetch %d: got %v, want ErrCorruptStorage", i, err)
		}
	}
	if idx.marks != 1 {
		t.Errorf("MarkFolderCorrupt called %d times, want 1 (gated per session)", idx.marks)
	}
	if !s.markedCorrupt {
		t.Error("session should record it already flagged INBOX")
	}
}
