package lmtp

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// recordingFTS captures what the delivery hook asked the service to index.
type recordingFTS struct {
	mu   sync.Mutex
	refs []fts.MailboxRef
	done chan struct{}
}

func (r *recordingFTS) Index(_ string, m fts.MailboxRef, _ uint32, _ int) error {
	r.mu.Lock()
	r.refs = append(r.refs, m)
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return nil
}
func (r *recordingFTS) seen() []fts.MailboxRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]fts.MailboxRef(nil), r.refs...)
}

func (r *recordingFTS) Prepend(string, fts.MailboxRef, uint32) error { return nil }
func (r *recordingFTS) Expunge(string, fts.MailboxRef, uint32) error { return nil }
func (r *recordingFTS) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{}, nil
}
func (r *recordingFTS) Status(string, fts.MailboxRef) (uint32, uint32, error) { return 0, 0, nil }
func (r *recordingFTS) Rescan(string, fts.MailboxRef) error                   { return nil }
func (r *recordingFTS) Optimize(string) error                                 { return nil }
func (r *recordingFTS) Close() error                                          { return nil }

// Delivered mail must reach the index, and the index is keyed by the folder's
// GUID (#1183). A reference carrying only the name is refused by the service,
// on a fire-and-forget path -- so the mail is silently unsearchable, which is
// what the sandbox found on 2.3.119 after every LMTP delivery.
func TestAutoindexCarriesTheFolderGUID(t *testing.T) {
	root := t.TempDir()
	mb := maildir.New()
	idx := fileindex.New()
	rec := &recordingFTS{done: make(chan struct{}, 4)}

	s := &session{opts: Options{
		Mailbox:      mb,
		Index:        idx,
		FTSClient:    rec,
		FTSAutoindex: true,
	}}

	info := &mailbox.UserInfo{
		Username: "alice@x", Home: filepath.Join(root, "alice"),
		MailPath: filepath.Join(root, "alice", "Maildir"),
	}
	box := mb.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ui := idx.OpenUser(info)
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	raw := "From: x@y\r\nSubject: probe\r\n\r\nbody\r\n"
	uid, folder, err := deliverOne(box, ui, "INBOX", bytes.NewReader([]byte(raw)), int64(len(raw)), nil, info.Username, "x@y", nil)
	if err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	if folder.GUID == ([16]byte{}) {
		t.Fatal("the delivered folder has no GUID, so the test cannot show one being passed")
	}
	s.ftsAutoindex(info.Username, folder, uid)

	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the delivery never reached the index")
	}
	refs := rec.seen()
	if len(refs) != 1 {
		t.Fatalf("indexed %d times, want 1", len(refs))
	}
	// Identity, not mere presence: the defect was a reference built from the
	// wrong thing, so any non-empty GUID would pass a presence check while
	// naming another folder -- or a message.
	if want := hex.EncodeToString(folder.GUID[:]); refs[0].GUID != want {
		t.Errorf("indexed GUID %q, want the delivered folder's %q", refs[0].GUID, want)
	}
	if refs[0].UIDValidity != folder.UIDValidity {
		t.Errorf("indexed UIDVALIDITY %d, want %d", refs[0].UIDValidity, folder.UIDValidity)
	}
	if refs[0].Name != "INBOX" {
		t.Errorf("indexed folder %q, want INBOX", refs[0].Name)
	}
}

// A folder without a GUID cannot be indexed at all, so the delivery says so
// once rather than firing a request the service will refuse.
func TestAutoindexSkipsAFolderWithoutGUID(t *testing.T) {
	rec := &recordingFTS{done: make(chan struct{}, 1)}
	s := &session{opts: Options{FTSClient: rec, FTSAutoindex: true}}

	s.ftsAutoindex("alice@x", mailbox.Folder{Name: "INBOX"}, 1)

	select {
	case <-rec.done:
		t.Error("asked the index about a folder with no identity")
	case <-time.After(200 * time.Millisecond):
	}
}
