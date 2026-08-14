package file

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A keyword is stored as a bit; the name the bit stands for lives in the
// extension header. A reader that keeps records it already holds must still
// re-read that header, or a bit set by another process decodes to no name and
// the keyword is silently gone — which is how a custom keyword set over IMAP
// became invisible over JMAP (#1278).
//
// Asserted here, in the layer that resolves bits to names. Routed through the
// JMAP surface it would measure the mapping there and the registry here at
// once, and pass whenever either one happened to be right.
func TestKeywordRegisteredByAnotherHandleIsVisibleAfterReload(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}

	// Two handles over one folder, as two pods are: separate Backends, so
	// neither shares the other's in-memory state.
	writer := New().OpenUser(info)
	defer writer.Close() //nolint:errcheck
	reader := New().OpenUser(info)
	defer reader.Close() //nolint:errcheck

	wf, err := writer.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("writer open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := writer.AllocateAndAppend(wf.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The reader opens the folder BEFORE the keyword exists, which is what
	// leaves it holding a registry that does not name it.
	rf, err := reader.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("reader open: %v", err)
	}
	if got := keywordsOfUID(t, reader, rf.ID, m.UID); len(got) != 0 {
		t.Fatalf("keywords before the write: %v", got)
	}

	const custom = "$smokelabel"
	if err := writer.UpdateFlags(wf.ID, m.UID, []string{`\Seen`}, []string{custom}); err != nil {
		t.Fatalf("update flags: %v", err)
	}

	got := keywordsOfUID(t, reader, rf.ID, m.UID)
	if len(got) != 1 || got[0] != custom {
		t.Errorf("keywords read by the second handle = %v, want [%s]", got, custom)
	}
	// The system flag travels in the record itself and was never the part at
	// risk; asserting it keeps the row honest about what it proved.
	msgs, err := reader.GetMessages(rf.ID, mailbox.SeqSet{{From: m.UID, To: m.UID}})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Flags) == 0 {
		t.Errorf("the system flag did not survive either: %+v", msgs)
	}
}

func keywordsOfUID(t *testing.T, idx mailbox.UserIndex, folderID uint64, uid uint32) []string {
	t.Helper()
	msgs, err := idx.GetMessages(folderID, mailbox.SeqSet{{From: uid, To: uid}})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("%d messages for uid %d", len(msgs), uid)
	}
	return msgs[0].Keywords
}

// The batch path is the one IMAP STORE actually takes (UpdateFlagsMulti), so
// it gets its own row: a fix applied to the single-message path only would
// leave the deployed path exactly as broken as before.
func TestKeywordSetThroughTheBatchPathIsVisibleToAnotherHandle(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}

	writer := New().OpenUser(info)
	defer writer.Close() //nolint:errcheck
	wf, err := writer.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("writer open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := writer.AllocateAndAppend(wf.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}

	const custom = "$smokelabel"
	if _, err := writer.UpdateFlagsMulti(wf.ID, map[uint32]mailbox.FlagsUpdate{
		m.UID: {Mode: mailbox.FlagsAdd, Flags: []string{`\Seen`}, Keywords: []string{custom}},
	}); err != nil {
		t.Fatalf("update flags multi: %v", err)
	}

	reader := New().OpenUser(info)
	defer reader.Close() //nolint:errcheck
	rf, err := reader.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("reader open: %v", err)
	}
	if got := keywordsOfUID(t, reader, rf.ID, m.UID); len(got) != 1 || got[0] != custom {
		t.Errorf("keywords read by a second handle = %v, want [%s]", got, custom)
	}
}

// A flag-only STORE must not start rewriting the base: the fix pays a base
// rewrite for keyword changes, and a row that let it spread to every \Seen
// would make the cost invisible.
func TestFlagOnlyStoreDoesNotRewriteTheBase(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: root}
	idx := New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m := &mailbox.MessageMeta{Filename: "f", Size: 100}
	if err := idx.AllocateAndAppend(f.ID, m); err != nil {
		t.Fatalf("append: %v", err)
	}

	base := filepath.Join(root, "yarilo.index")
	before, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base: %v", err)
	}
	if _, err := idx.UpdateFlagsMulti(f.ID, map[uint32]mailbox.FlagsUpdate{
		m.UID: {Mode: mailbox.FlagsAdd, Flags: []string{`\Seen`}},
	}); err != nil {
		t.Fatalf("update flags multi: %v", err)
	}
	after, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("a flag-only STORE rewrote the base; the keyword cost leaked onto the hot path")
	}
}
