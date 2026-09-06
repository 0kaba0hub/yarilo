package dboxv2

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The reference names an sdbox message u.<uid> -- its own fixtures are u.1 and
// u.2 -- and a store of u.<guid> names is one it cannot read (#1704). The name
// is settled inside the same cycle that hands out the uid.
func TestASavedMessageIsNamedByItsUID(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	saved, _, guid, err := mb.Save("INBOX", strings.NewReader("msg\n"), 0, 4, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	namer, ok := mb.(mailbox.UIDNamer)
	if !ok {
		t.Fatal("the sdbox driver does not name a message by its uid")
	}
	appender, ok := idx.(mailbox.NamingAppender)
	if !ok {
		t.Fatal("the index cannot settle a name inside the cycle that allocates the uid")
	}
	m := &mailbox.MessageMeta{Size: 4, VSize: 4, GUID: guid}
	if err := appender.AllocateAndAppendNamed(folder.ID, m, func(uid uint32) (string, error) {
		return namer.AssignUID("INBOX", saved, uid)
	}); err != nil {
		t.Fatal(err)
	}

	want := "u." + strconv.FormatUint(uint64(m.UID), 10)
	if m.Filename != want {
		t.Errorf("the record names %q, want %q", m.Filename, want)
	}
	entries, err := os.ReadDir(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			got = append(got, e.Name())
		}
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("the folder holds %v, want [%s]", got, want)
	}
}

// saveNamed is the two-step a save now takes: the body first, its name once a
// uid exists. Tests that only need a stored message use it.
func saveNamed(t *testing.T, mb mailbox.UserMailbox, folder, body string, uid uint32, guid [16]byte) (string, uint32) {
	t.Helper()
	temp, vsize, g, err := mb.Save(folder, strings.NewReader(body), 0, int64(len(body)), nil, guid)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	_ = g
	name, err := mb.(mailbox.UIDNamer).AssignUID(folder, temp, uid)
	if err != nil {
		t.Fatalf("assign uid %d: %v", uid, err)
	}
	return name, vsize
}

// A store this server wrote before #1704 is full of names the reference cannot
// read; opening it renames them and the index follows.
func TestAGUIDNamedStoreIsMigrated(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")

	// The old shape: the file named by its GUID, the index pointing at it.
	var guids []string
	for uid := uint32(1); uid <= 3; uid++ {
		temp, vsize, guid, serr := mb.Save("INBOX", strings.NewReader("msg\n"), 0, 4, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		old := "u." + guidHex(guid)
		if rerr := os.Rename(filepath.Join(dir, temp), filepath.Join(dir, old)); rerr != nil {
			t.Fatal(rerr)
		}
		guids = append(guids, old)
		if aerr := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
			UID: uid, Filename: old, Size: 4, VSize: vsize, GUID: guid,
		}); aerr != nil {
			t.Fatal(aerr)
		}
	}

	migrator, ok := mb.(interface {
		MigrateUIDNames(mailbox.UserIndex, *mailbox.Folder) (int, error)
	})
	if !ok {
		t.Fatal("the sdbox driver cannot migrate the names it wrote")
	}
	n, err := migrator.MigrateUIDNames(idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("migrated %d messages, want 3", n)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	if want := []string{"u.1", "u.2", "u.3"}; !equalStrings(got, want) {
		t.Errorf("the folder holds %v, want %v -- a name the reference cannot read survived", got, want)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		want := "u." + strconv.FormatUint(uint64(m.UID), 10)
		if m.Filename != want {
			t.Errorf("uid %d still names %q, want %q", m.UID, m.Filename, want)
		}
	}
	// And the file each record names can be read: the rename and the record agree.
	for _, m := range msgs {
		rc, ferr := mb.Fetch("INBOX", m.Filename, false)
		if ferr != nil {
			t.Errorf("uid %d: %v", m.UID, ferr)
			continue
		}
		rc.Close() //nolint:errcheck
	}
	_ = guids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// appendRecorder records what an APPEND takes and, on release, what the folder
// then holds: the two together say whether the name was settled inside.
type appendRecorder struct {
	mu     sync.Mutex
	taken  []string
	atFree []string
	dir    string
}

func (l *appendRecorder) Lock(_ context.Context, resource, _ string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.taken = append(l.taken, resource)
	return locks.Lock{ID: resource, Resource: resource}, nil
}

func (l *appendRecorder) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}

func (l *appendRecorder) Unlock(_ context.Context, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, _ := os.ReadDir(l.dir)
	l.atFree = nil
	for _, e := range entries {
		l.atFree = append(l.atFree, e.Name())
	}
	return nil
}
func (l *appendRecorder) Renew(context.Context, string, time.Duration) error { return nil }
func (l *appendRecorder) HoldsResource(string) bool                          { return false }
func (l *appendRecorder) Close() error                                       { return nil }
func (l *appendRecorder) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *appendRecorder) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *appendRecorder) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

// One APPEND takes the folder's key once, and by the time it is released the
// message already wears the name its uid gives it (#1704).
func TestAnAppendTakesTheFolderKeyOnce(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	rec := &appendRecorder{dir: dir}
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: home}
	mb := New(WithLocker(rec)).OpenUser(info)
	if err := mb.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New(fileidx.WithLocker(rec)).OpenUser(info)
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	rec.taken = nil
	rec.mu.Unlock()

	temp, vsize, guid, err := mb.Save("INBOX", strings.NewReader("msg\n"), 0, 4, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Filename: temp, Size: 4, VSize: vsize, GUID: guid}
	if err := mailbox.RecordSaved(idx, mb, folder.ID, "INBOX", m); err != nil {
		t.Fatal(err)
	}

	key := locks.MailboxKey("alice@example.com", "INBOX")
	got := 0
	for _, r := range rec.taken {
		if r == key {
			got++
		}
	}
	if got != 1 {
		t.Errorf("the append took %q %d times, want once: %v", key, got, rec.taken)
	}
	want := "u." + strconv.FormatUint(uint64(m.UID), 10)
	found := false
	for _, n := range rec.atFree {
		if n == want {
			found = true
		}
	}
	if !found {
		t.Errorf("at release the folder held %v, and none of it is %s", rec.atFree, want)
	}
}

// Every path that puts a file in a folder names it u.<uid> -- the shape the
// reference's own fixtures carry (sdbox-u.1, sdbox-u.2). One path still minting
// a name from a GUID would leave a store it cannot read (#1704).
func TestNoPathWritesAGUIDName(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	if err := mb.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	inbox, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Saved.
	temp, vsize, guid, err := mb.Save("INBOX", strings.NewReader("msg\n"), 0, 4, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Filename: temp, Size: 4, VSize: vsize, GUID: guid}
	if err := mailbox.RecordSaved(idx, mb, inbox.ID, "INBOX", m); err != nil {
		t.Fatal(err)
	}
	// Copied.
	copier, ok := mb.(interface {
		Copy(srcFolder, srcFilename, dstFolder string, dstUID uint32) (string, error)
	})
	if !ok {
		t.Fatal("the sdbox driver no longer copies")
	}
	if _, err := copier.Copy("INBOX", m.Filename, "Archive", 9); err != nil {
		t.Fatal(err)
	}
	// Moved, into a folder where that very name is taken: the collision path.
	second, _ := saveNamed(t, mb, "INBOX", "other\n", 2, [16]byte{})
	if _, cerr := copier.Copy("INBOX", second, "Archive", 2); cerr != nil {
		t.Fatal(cerr)
	}
	moved, _, err := mb.Move("INBOX", "Archive", second, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	// A collision parks the file, and the destination's uid names it.
	archive, err := idx.OpenFolder("Archive", 1)
	if err != nil {
		t.Fatal(err)
	}
	nm := &mailbox.MessageMeta{Filename: moved, Size: 6, VSize: 6}
	if err := mailbox.RecordSaved(idx, mb, archive.ID, "Archive", nm); err != nil {
		t.Fatal(err)
	}

	for _, folder := range []string{"INBOX", "Archive"} {
		entries, rerr := os.ReadDir(filepath.Join(home, "sdbox", "mailboxes", folder, "dbox-Mails"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			rest, ok := strings.CutPrefix(e.Name(), "u.")
			if !ok {
				t.Errorf("%s/%s is named by neither uid nor anything the reference reads", folder, e.Name())
				continue
			}
			if _, cerr := strconv.ParseUint(rest, 10, 32); cerr != nil {
				t.Errorf("%s/%s: %q is not a uid", folder, e.Name(), rest)
			}
		}
	}
}

// A save that never got a uid is a crash's leftover once it is old enough; one
// still young is a save in flight and must survive (#1704).
func TestOnlyAnOldTempIsSweptAway(t *testing.T) {
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")

	old, _, _, err := mb.Save("INBOX", strings.NewReader("old\n"), 0, 4, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-staleTemp - time.Hour)
	if err := os.Chtimes(filepath.Join(dir, old), long, long); err != nil {
		t.Fatal(err)
	}
	young, _, _, err := mb.Save("INBOX", strings.NewReader("young\n"), 0, 6, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}

	migrator := mb.(interface {
		MigrateUIDNames(mailbox.UserIndex, *mailbox.Folder) (int, error)
	})
	if _, err := migrator.MigrateUIDNames(idx, folder); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Errorf("the old temp survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, young)); err != nil {
		t.Errorf("a save in flight was swept away: %v", err)
	}
}
