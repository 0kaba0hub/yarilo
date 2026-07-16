package reconcile_test

import (
	"path/filepath"
	"strings"
	"testing"

	fileidx "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/internal/storage/reconcile"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

func testHome(root, user string) string {
	if at := strings.LastIndex(user, "@"); at >= 0 {
		return filepath.Join(root, user[at+1:], user[:at])
	}
	return filepath.Join(root, user)
}

// setup wires a real maildir box and a real fileindex over the same home so the
// reconcile core is exercised end to end (scan + ResetFolder + quota recompute).
func setup(t *testing.T) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	root := t.TempDir()
	const user = "u@x.com"
	home := testHome(root, user)
	info := &mailbox.UserInfo{Username: user, Home: home}

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	idx := fileidx.New().OpenUser(info)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	return box, idx, folder
}

// save writes a message file into cur/ (a real delivery) and returns its
// filename. When index is true the record is also appended to the index with
// the given flags — a message yarilo already tracks. When false the file looks
// like an out-of-band delivery the index has never seen.
func save(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, folderID uint64, uid uint32, flags []string, track bool) string {
	t.Helper()
	name, err := box.Save("INBOX", strings.NewReader("body\n"), uid, 5, nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if track {
		if err := idx.AppendMessage(folderID, &mailbox.MessageMeta{
			UID: uid, Filename: name, Size: 5, VSize: 5, Flags: flags,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	return name
}

func names(msgs []*mailbox.MessageMeta) map[string]*mailbox.MessageMeta {
	m := make(map[string]*mailbox.MessageMeta, len(msgs))
	for _, x := range msgs {
		m[x.Filename] = x
	}
	return m
}

func hasFlag(m *mailbox.MessageMeta, flag string) bool {
	for _, f := range m.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

func TestSyncNew_ImportsExpungesAndPreservesFlags(t *testing.T) {
	box, idx, folder := setup(t)

	// Two tracked messages (one \Seen only in the index, filename carries no
	// flags) plus one out-of-band delivery the index has never seen.
	tracked := save(t, box, idx, folder.ID, 1, []string{"\\Seen"}, true)
	_ = save(t, box, idx, folder.ID, 2, nil, true)
	external := save(t, box, idx, folder.ID, 0, nil, false)

	folder, _ = idx.OpenFolder("INBOX", 1)
	st, err := reconcile.SyncNew(box, idx, folder)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !st.Changed || st.Imported != 1 || st.Expunged != 0 {
		t.Fatalf("first sync stats = %+v, want Changed imported=1 expunged=0", st)
	}

	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 3 {
		t.Fatalf("after import: %d messages, want 3", len(msgs))
	}
	byName := names(msgs)
	// The externally delivered file gained a fresh UID above the previous max.
	if got := byName[external]; got == nil || got.UID != 3 {
		t.Fatalf("external UID = %v, want 3", byName[external])
	}
	// Concern #3: the tracked message keeps its index flag; the reconcile must
	// NOT revert it to the (flagless) filename trailer.
	if got := byName[tracked]; got == nil || !hasFlag(got, "\\Seen") {
		t.Fatalf("tracked message lost its \\Seen flag: %+v", byName[tracked])
	}

	// Idempotent: nothing changed on disk => no write, no stat drift.
	folder, _ = idx.OpenFolder("INBOX", 1)
	if st, err := reconcile.SyncNew(box, idx, folder); err != nil || st.Changed {
		t.Fatalf("second sync = %+v, err=%v, want no change", st, err)
	}

	// Remove a tracked file out of band => it must be expunged from the index.
	if err := box.Remove("INBOX", tracked); err != nil {
		t.Fatalf("remove: %v", err)
	}
	folder, _ = idx.OpenFolder("INBOX", 1)
	st, err = reconcile.SyncNew(box, idx, folder)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if !st.Changed || st.Expunged != 1 || st.Imported != 0 {
		t.Fatalf("third sync stats = %+v, want expunged=1 imported=0", st)
	}
	msgs, _ = idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if _, gone := names(msgs)[tracked]; gone {
		t.Fatalf("expunged file still indexed")
	}
	if len(msgs) != 2 {
		t.Fatalf("after expunge: %d messages, want 2", len(msgs))
	}
}
