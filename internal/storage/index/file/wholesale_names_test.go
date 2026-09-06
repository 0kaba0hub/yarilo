package file

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// openUserAt opens a second view of the same store, the way a second session
// does: its own folderState over the same directory.
func openUserAt(t *testing.T, root, session string) *userIndex {
	t.Helper()
	ui := New().OpenUser(&mailbox.UserInfo{
		Username: testUser, Home: testHome(root, testUser), SessionID: session,
	}).(*userHandle).ui
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck
	return ui
}

// folderStateFor reaches the open state for a folder, so a test can drive the
// wholesale flush the way stampLineage does.
func (u *userIndex) folderStateFor(t *testing.T, folder string) *folderState {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, fs := range u.open {
		if fs.folder == folder {
			return fs
		}
	}
	t.Fatalf("folder %q not open", folder)
	return nil
}

// A record returns from the log, a name does not: a wholesale rewrite from one
// state's map dropped a name another state had just appended (#1693).
func TestAWholesaleRewriteKeepsANameAnotherStateJustWrote(t *testing.T) {
	home := t.TempDir()
	a := openUserAt(t, home, "a")
	fa, err := a.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: 1, Filename: "m.1", Size: 10}); err != nil {
		t.Fatal(err)
	}

	// The second state reads the folder as it is now, and does not look again.
	b := openUserAt(t, home, "b")
	if _, err := b.OpenFolder("INBOX", 0, ""); err != nil {
		t.Fatal(err)
	}

	if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: 2, Filename: "m.2", Size: 20}); err != nil {
		t.Fatal(err)
	}

	// A wholesale flush with no reload before it: what stampLineage does.
	fsb := b.folderStateFor(t, "INBOX")
	if err := fsb.flush(true); err != nil {
		t.Fatal(err)
	}

	c := openUserAt(t, home, "c")
	fc, err := c.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := c.GetMessages(fc.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint32]string{}
	for _, m := range msgs {
		got[m.UID] = m.Filename
	}
	if got[2] != "m.2" {
		t.Errorf("uid 2 names %q, want \"m.2\": the wholesale rewrite dropped a name it did not have in memory", got[2])
	}
	if got[1] != "m.1" {
		t.Errorf("uid 1 names %q, want \"m.1\"", got[1])
	}
}

// The index's records are the authority: a uid it no longer carries is not
// carried into the new sidecar either, or an expunge would be undone.
func TestAWholesaleRewriteDropsTheNameOfAnExpungedRecord(t *testing.T) {
	home := t.TempDir()
	a := openUserAt(t, home, "a")
	f, err := a.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= 2; uid++ {
		m := &mailbox.MessageMeta{UID: uid, Filename: "m." + string(rune('0'+uid)), Size: 10}
		if err := a.AppendMessage(f.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ExpungeMessage(f.ID, 1); err != nil {
		t.Fatal(err)
	}
	fs := a.folderStateFor(t, "INBOX")
	if err := fs.flush(true); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(fs.indexDir, IndexNamesFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(line, "1\t") {
			t.Errorf("the sidecar still names expunged uid 1: %q", line)
		}
	}
}

// The only way left to hold a record no reader can resolve. Not a caller's
// defect -- a migrated index is named later -- so a line, not a panic.
func TestALiveRecordWithNoNameAnywhereIsReported(t *testing.T) {
	home := t.TempDir()
	a := openUserAt(t, home, "a")
	f, err := a.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "m.1", Size: 10}); err != nil {
		t.Fatal(err)
	}
	fs := a.folderStateFor(t, "INBOX")
	if fs.namesFD != nil {
		fs.namesFD.Close() //nolint:errcheck
		fs.namesFD = nil
	}
	if err := os.Remove(filepath.Join(fs.indexDir, IndexNamesFileName)); err != nil {
		t.Fatal(err)
	}
	delete(fs.filenames, 1)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	if err := fs.flush(true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "named nowhere") {
		t.Fatalf("a live record with no name anywhere was written out in silence: %q", buf.String())
	}
}

// lockRecorder records what the index takes and gives back, so a sidecar read
// can be placed inside a hold or outside it.
type lockRecorder struct {
	mu     sync.Mutex
	events []string
}

func (l *lockRecorder) note(ev string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *lockRecorder) Lock(_ context.Context, resource, _ string, _ time.Duration) (locks.Lock, error) {
	l.note("lock " + resource)
	return locks.Lock{ID: resource, Resource: resource}, nil
}

func (l *lockRecorder) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}
func (l *lockRecorder) Unlock(_ context.Context, id string) error {
	l.note("unlock " + id)
	return nil
}
func (l *lockRecorder) Renew(context.Context, string, time.Duration) error { return nil }
func (l *lockRecorder) HoldsResource(string) bool                          { return false }
func (l *lockRecorder) Close() error                                       { return nil }
func (l *lockRecorder) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *lockRecorder) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *lockRecorder) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

func (l *lockRecorder) taken() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

// The merge reads the sidecar and rewrites it: outside the folder's exclusive
// lock that is a new window, so every read sits inside a hold (#1693).
func TestTheSidecarIsOnlyReadUnderTheFolderLock(t *testing.T) {
	rec := &lockRecorder{}
	ui := New(WithLocker(rec)).OpenUser(&mailbox.UserInfo{
		Username: testUser, Home: testHome(t.TempDir(), testUser), SessionID: "s",
	}).(*userHandle).ui
	defer ui.Close() //nolint:errcheck

	beforeNameMerge = func() { rec.note("read names") }
	defer func() { beforeNameMerge = nil }()

	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Filename: "m.1", Size: 10}); err != nil {
		t.Fatal(err)
	}
	fs := ui.folderStateFor(t, "INBOX")
	// Through the lock, the way stampLineage and createFresh reach a wholesale
	// flush.
	if err := ui.withFolderLock(fs, func() error { return fs.flush(true) }); err != nil {
		t.Fatal(err)
	}

	held := 0
	reads := 0
	for _, ev := range rec.taken() {
		switch {
		case strings.HasPrefix(ev, "lock "):
			held++
		case strings.HasPrefix(ev, "unlock "):
			held--
		case ev == "read names":
			reads++
			if held == 0 {
				t.Errorf("the sidecar was read with no lock held: %v", rec.taken())
			}
		}
	}
	if reads == 0 {
		t.Fatal("nothing read the sidecar: the row proves nothing")
	}
}
