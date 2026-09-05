package imap

import (
	"errors"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// markingIndex records whether the folder was flagged corrupt, and answers
// GetMessages from a set the test controls.
type markingIndex struct {
	mailbox.UserIndex
	present map[uint32]bool
	marked  int
	getErr  error
}

func (i *markingIndex) GetMessages(_ uint64, uids mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	if i.getErr != nil {
		return nil, i.getErr
	}
	var out []*mailbox.MessageMeta
	for _, r := range uids {
		for uid := r.From; uid <= r.To; uid++ {
			if i.present[uid] {
				out = append(out, &mailbox.MessageMeta{UID: uid})
			}
		}
	}
	return out, nil
}

func (i *markingIndex) MarkFolderCorrupt(uint64) error  { i.marked++; return nil }
func (i *markingIndex) ClearFolderCorrupt(uint64) error { return nil }

// A read that lost to another session's expunge marks nothing: sdbox calls a
// vanished file corruption, and under load that is what an expunge is (#1690).
func TestAReadThatLostToAnExpungeMarksNothing(t *testing.T) {
	idx := &markingIndex{present: map[uint32]bool{}} // uid 7 already expunged
	s := &session{userInfo: &mailbox.UserInfo{Username: "u@example.com"}}
	err := errors.Join(errors.New("open: no such file"), mailbox.ErrCorruptStorage)

	s.flagCorruptOnRead(idx, 1, "INBOX", "u.7", 7, err)
	if idx.marked != 0 {
		t.Errorf("the folder was marked corrupt %d times: a message another session "+
			"expunged is not damage", idx.marked)
	}
}

// A record whose file went away with the record still in the index is damage,
// and still marks: that is the case the heal exists for.
func TestARecordWithNoFileStillMarks(t *testing.T) {
	idx := &markingIndex{present: map[uint32]bool{7: true}}
	s := &session{userInfo: &mailbox.UserInfo{Username: "u@example.com"}}
	err := errors.Join(errors.New("open: no such file"), mailbox.ErrCorruptStorage)

	s.flagCorruptOnRead(idx, 1, "INBOX", "u.7", 7, err)
	if idx.marked != 1 {
		t.Errorf("the folder was marked %d times, want 1: a dangling record is what the "+
			"reactive heal is for", idx.marked)
	}
}

// An index that cannot answer keeps the old behaviour: a read fault must not be
// swallowed because the check itself failed.
func TestAnUnreadableIndexStillMarks(t *testing.T) {
	idx := &markingIndex{getErr: errors.New("index unavailable")}
	s := &session{userInfo: &mailbox.UserInfo{Username: "u@example.com"}}
	err := errors.Join(errors.New("open: no such file"), mailbox.ErrCorruptStorage)

	s.flagCorruptOnRead(idx, 1, "INBOX", "u.7", 7, err)
	if idx.marked != 1 {
		t.Errorf("the folder was marked %d times, want 1", idx.marked)
	}
}

// The re-read sees another backend's expunge from disk: the rows above prove
// only the wiring, against a double (#1690).
func TestTheReReadSeesAnotherHandlesExpunge(t *testing.T) {
	dir := t.TempDir()
	info := func() *mailbox.UserInfo {
		return &mailbox.UserInfo{Username: "u@example.com", Home: dir, SessionID: "s"}
	}
	// Two backends: two index instances over one directory, as two sessions in
	// two processes have.
	a := fileindex.New().OpenUser(info())
	defer a.Close() //nolint:errcheck
	b := fileindex.New().OpenUser(info())
	defer b.Close() //nolint:errcheck

	fa, err := a.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{1, 2} {
		if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{
			UID: uid, Filename: "m." + string(rune('0'+uid)), Size: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fb, err := b.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ExpungeMessage(fb.ID, 1); err != nil {
		t.Fatal(err)
	}

	s := &session{userInfo: info()}
	if s.recordStillThere(a, fa.ID, 1) {
		t.Error("the re-read still sees a record another handle expunged: a read that lost " +
			"the race would mark the folder corrupt")
	}
	if !s.recordStillThere(a, fa.ID, 2) {
		t.Error("the re-read lost a record nobody touched")
	}
}
