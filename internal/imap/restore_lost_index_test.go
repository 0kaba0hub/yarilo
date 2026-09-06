package imap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// sdboxWithMail builds a real sdbox store with n messages in INBOX and returns
// the handles a session would hold.
func sdboxWithMail(t *testing.T, n int) (*mailbox.UserInfo, mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: t.TempDir(), Driver: "sdbox"}
	box := dboxv2.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		body := "From: a@b\r\nSubject: s\r\n\r\nbody\r\n"
		temp, _, _, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		name := temp
		if namer, ok := mailbox.Driver(box).(mailbox.UIDNamer); ok {
			if name, serr = namer.AssignUID("INBOX", temp, uint32(i+1)); serr != nil {
				t.Fatal(serr)
			}
		}
		if aerr := idx.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Filename: name}); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return info, box, idx, f
}

func dropOurIndexes(t *testing.T, home string) int {
	t.Helper()
	removed := 0
	err := filepath.Walk(home, func(p string, fi os.FileInfo, e error) error {
		if e != nil || fi.IsDir() {
			return nil //nolint:nilerr
		}
		if strings.HasPrefix(filepath.Base(p), "yarilo.index") {
			if rerr := os.Remove(p); rerr != nil {
				t.Fatal(rerr)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatal("no index files were removed, so the test proves nothing")
	}
	return removed
}

// A folder whose index is gone and whose messages are still in storage is
// rebuilt from the files rather than served as an empty mailbox (#1608).
func TestAFolderWhoseIndexIsLostIsRebuiltFromStorage(t *testing.T) {
	info, box, idx, _ := sdboxWithMail(t, 3)
	idx.Close() //nolint:errcheck
	dropOurIndexes(t, info.Home)

	idx2 := indexfile.New().OpenUser(info)
	defer idx2.Close() //nolint:errcheck
	f, err := idx2.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	// This is the state the window reported: an empty mailbox over a full
	// directory.
	if f.Messages != 0 {
		t.Fatalf("the folder opened with %d messages; the test needs the lost-index state", f.Messages)
	}

	s := &session{srv: &Server{opts: Options{}}}
	h := &nsHandle{box: box, idx: idx2}
	refreshed := s.dboxRestoreIfIndexLost(h, "INBOX", f)
	if refreshed == nil {
		t.Fatal("the folder was left empty although its messages are in storage")
	}
	if refreshed.Messages != 3 {
		t.Errorf("after the rebuild the folder holds %d messages, want 3", refreshed.Messages)
	}

	msgs, err := idx2.GetMessages(refreshed.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		rc, ferr := box.Fetch("INBOX", m.Filename, m.AltTier)
		if ferr != nil {
			t.Errorf("uid %d: the rebuilt record does not read: %v", m.UID, ferr)
			continue
		}
		rc.Close() //nolint:errcheck
	}
}

// The counterpart, and the reason the trigger is not just "no messages": an
// empty folder over empty storage is an ordinary new folder and stays one.
func TestAnEmptyFolderOverEmptyStorageIsNotRebuilt(t *testing.T) {
	info, box, idx, f := sdboxWithMail(t, 0)
	defer idx.Close() //nolint:errcheck

	s := &session{srv: &Server{opts: Options{}}}
	h := &nsHandle{box: box, idx: idx}
	if refreshed := s.dboxRestoreIfIndexLost(h, "INBOX", f); refreshed != nil {
		t.Errorf("a new folder was rebuilt: %+v", refreshed)
	}
	_ = info
}
