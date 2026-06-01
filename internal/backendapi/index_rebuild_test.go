package backendapi

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// TestRebuildRecoversAfterIndexLoss simulates the canonical
// "operator deleted the .index file by mistake" scenario: deliver a
// few messages so storage + index are both populated, blow away the
// index (via ResetFolder with no records), then drive a rebuild
// over backend-api. Expectation: the fileindex now matches the
// on-disk maildir again and UIDs are freshly assigned.
func TestRebuildRecoversAfterIndexLoss(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"

	// Init mailbox + deliver three messages through the storage
	// driver, then index them so the system is in a consistent
	// state we can later deliberately break.
	uc, err := newAdminUserContext(t, ts, root, user)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()

	for i := 0; i < 3; i++ {
		uc.deliver(t, "Subject: msg "+string(rune('A'+i))+"\r\n\r\nbody\r\n")
	}

	// Confirm the index has 3 records.
	if got := uc.indexCount(t); got != 3 {
		t.Fatalf("pre-rebuild index count = %d, want 3", got)
	}

	// Wipe the index by calling ResetFolder with no records.
	if err := uc.idx.ResetFolder(uc.folder.ID, nil); err != nil {
		t.Fatalf("simulate index loss: %v", err)
	}
	if got := uc.indexCount(t); got != 0 {
		t.Fatalf("after wipe index count = %d, want 0", got)
	}

	// Trigger rebuild via the HTTP API.
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/rebuild", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("rebuild status=%d body=%s", status, body)
	}
	var stats struct {
		Scanned       int `json:"scanned"`
		UIDsPreserved int `json:"uids_preserved"`
		UIDsAssigned  int `json:"uids_assigned"`
	}
	decodeJSONBody(t, body, &stats)
	if stats.Scanned != 3 {
		t.Errorf("scanned=%d want 3", stats.Scanned)
	}
	if stats.UIDsAssigned != 3 {
		t.Errorf("uids_assigned=%d want 3 (nothing to preserve)", stats.UIDsAssigned)
	}

	// Verify the index now has 3 records again.
	if got := uc.indexCount(t); got != 3 {
		t.Errorf("post-rebuild index count = %d, want 3", got)
	}
}

// TestRebuildPreservesUIDsForKnownFilenames is the happy-path
// preserve scenario: storage + index are already consistent, run
// rebuild; every filename should match an existing index entry so
// UIDs stay put and `uids_preserved == scanned`.
func TestRebuildPreservesUIDsForKnownFilenames(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"
	uc, err := newAdminUserContext(t, ts, root, user)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()

	for i := 0; i < 2; i++ {
		uc.deliver(t, "msg")
	}
	before := uc.uidsByFilename(t)

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/rebuild", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var stats struct {
		UIDsPreserved int `json:"uids_preserved"`
		UIDsAssigned  int `json:"uids_assigned"`
	}
	decodeJSONBody(t, body, &stats)
	if stats.UIDsPreserved != 2 || stats.UIDsAssigned != 0 {
		t.Errorf("preserved=%d assigned=%d want 2/0", stats.UIDsPreserved, stats.UIDsAssigned)
	}
	after := uc.uidsByFilename(t)
	for fname, uid := range before {
		if after[fname] != uid {
			t.Errorf("uid drift for %s: before=%d after=%d", fname, uid, after[fname])
		}
	}
}

// TestRebuildMdboxReturns501 confirms the explicit deferral path
// returns 501 with a hint pointing operators at the right TODO.
func TestRebuildMdboxReturns501(t *testing.T) {
	ts, _ := storageTestServerMdbox(t)
	const user = "alice@example.com"

	// Trigger Init + Folder creation via the existing folder/list
	// path so the mdbox storage root exists before scan runs.
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/rebuild", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501; body=%s", status, body)
	}
	if !strings.Contains(string(body), "MDBOX-PROD-READY") {
		t.Errorf("body=%s want pointer to MDBOX-PROD-READY", body)
	}
}

// TestOptimizeIsNoopOnEmptyLog walks the empty-log fast-path —
// optimize on a fresh folder must return 200 with a duration
// stat, no error.
func TestOptimizeIsNoopOnEmptyLog(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/optimize", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

// TestFolderRepairCallsBothSteps drives the rebuild+optimize
// combo and checks the response carries both substats.
func TestFolderRepairCallsBothSteps(t *testing.T) {
	ts, root := storageTestServer(t)
	const user = "alice@example.com"
	uc, err := newAdminUserContext(t, ts, root, user)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.cleanup()
	uc.deliver(t, "msg")

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/repair", "",
		map[string]any{"user": user, "folder": "INBOX"})
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		Rebuild  map[string]any `json:"rebuild"`
		Optimize map[string]any `json:"optimize"`
	}
	decodeJSONBody(t, body, &resp)
	if resp.Rebuild == nil || resp.Optimize == nil {
		t.Errorf("resp missing one of rebuild/optimize: %+v", resp)
	}
}

// ---- helpers --------------------------------------------------------------

// adminUserContext gives the rebuild tests a thin handle that
// drives the underlying storage backends directly (skipping the
// HTTP layer) so we can populate state before rebuild and inspect
// it after. Mirrors what backend-api itself does at runtime.
type adminUserContext struct {
	t       *testing.T
	box     mailbox.UserMailbox
	idx     mailbox.UserIndex
	folder  *mailbox.Folder
	user    string
	cleanup func()
}

func newAdminUserContext(t *testing.T, _ any, root, user string) (*adminUserContext, error) {
	t.Helper()
	// Build storage backends mirroring storageTestServer wiring.
	mb, idx := newMaildirAndIndexAt(t, root)
	info := &mailbox.UserInfo{Username: user, Home: maildirHome(root, user)}
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		return nil, err
	}
	u := idx.OpenUser(info)
	folder, err := u.OpenFolder("INBOX", 0)
	if err != nil {
		return nil, err
	}
	return &adminUserContext{
		t:      t,
		box:    box,
		idx:    u,
		folder: folder,
		user:   user,
		cleanup: func() {
			_ = box.Close()
			_ = u.Close()
		},
	}, nil
}

func (a *adminUserContext) deliver(t *testing.T, body string) {
	t.Helper()
	uid, err := a.idx.AllocateUID(a.folder.ID)
	if err != nil {
		t.Fatalf("allocateUID: %v", err)
	}
	filename, err := a.box.Save("INBOX", io.NopCloser(bytes.NewBufferString(body)), uid, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := a.idx.AppendMessage(a.folder.ID, &mailbox.MessageMeta{
		UID:      uid,
		Filename: filename,
		Size:     uint32(len(body)),
	}); err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
}

// indexCount and uidsByFilename re-OpenFolder so they observe the
// freshest on-disk state — the HTTP rebuild path opens its own
// handles, so the test's original in-memory index state can diverge.
func (a *adminUserContext) indexCount(t *testing.T) int {
	t.Helper()
	fresh, err := a.idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("reopen folder: %v", err)
	}
	msgs, err := a.idx.GetMessages(fresh.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("getMessages: %v", err)
	}
	return len(msgs)
}

func (a *adminUserContext) uidsByFilename(t *testing.T) map[string]uint32 {
	t.Helper()
	fresh, err := a.idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("reopen folder: %v", err)
	}
	msgs, err := a.idx.GetMessages(fresh.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("getMessages: %v", err)
	}
	out := make(map[string]uint32, len(msgs))
	for _, m := range msgs {
		out[m.Filename] = m.UID
	}
	return out
}
