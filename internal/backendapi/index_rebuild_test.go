package backendapi

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
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
	if _, err := uc.idx.ResetFolder(uc.folder.ID, nil); err != nil {
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

// TestRebuildMdboxRejected verifies the per-folder rebuild refuses mdbox: its
// scan is storage-wide (folder-agnostic), so running RebuildFolder would import
// every stored message into the target folder with fresh UIDs. The endpoint must
// return 501 until the storage-wide rebuild lands (#594 Phase 2b), never fall
// through to the destructive per-folder path.
func TestRebuildMdboxRejected(t *testing.T) {
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
}

// TestStorageRebuildEndpointMdbox drives the storage-wide rebuild endpoint for
// mdbox: it must succeed (200) and report a bumped generation counter.
func TestStorageRebuildEndpointMdbox(t *testing.T) {
	ts, _ := storageTestServerMdbox(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/rebuild-storage", "",
		map[string]any{"user": user})
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", status, body)
	}
	var stats struct {
		RebuildCount int `json:"rebuild_count"`
	}
	decodeJSONBody(t, body, &stats)
	if stats.RebuildCount != 1 {
		t.Errorf("rebuild_count=%d want 1", stats.RebuildCount)
	}
}

// TestStorageRebuildEndpointRejectsNonMdbox verifies the storage-wide endpoint
// refuses a folder-per-file driver (maildir), which must use per-folder rebuild.
func TestStorageRebuildEndpointRejectsNonMdbox(t *testing.T) {
	ts, _ := storageTestServer(t)
	const user = "alice@example.com"
	doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/index/rebuild-storage", "",
		map[string]any{"user": user})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", status, body)
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
	info    *mailbox.UserInfo
	root    string
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
		info:   info,
		root:   root,
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
	filename, _, err := a.box.Save("INBOX", io.NopCloser(bytes.NewBufferString(body)), uid, int64(len(body)), nil)
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

// indexCount and uidsByFilename open a fresh userIndex handle each call
// so they always read the current on-disk state without relying on the
// mtime-based reload cache of the long-lived uc.idx handle.
func (a *adminUserContext) indexCount(t *testing.T) int {
	t.Helper()
	_, idx := newMaildirAndIndexAt(t, a.root)
	u := idx.OpenUser(a.info)
	defer func() { _ = u.Close() }()
	f, err := u.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("indexCount/open: %v", err)
	}
	msgs, err := u.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("indexCount/getMessages: %v", err)
	}
	return len(msgs)
}

func (a *adminUserContext) uidsByFilename(t *testing.T) map[string]uint32 {
	t.Helper()
	_, idx := newMaildirAndIndexAt(t, a.root)
	u := idx.OpenUser(a.info)
	defer func() { _ = u.Close() }()
	f, err := u.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("uidsByFilename/open: %v", err)
	}
	msgs, err := u.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("uidsByFilename/getMessages: %v", err)
	}
	out := make(map[string]uint32, len(msgs))
	for _, m := range msgs {
		out[m.Filename] = m.UID
	}
	return out
}
