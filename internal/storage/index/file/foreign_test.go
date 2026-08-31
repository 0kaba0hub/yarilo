package file_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// foreignStore lays out a store exactly as another implementation left it:
// their folder index and log with the messages, their map log in storage/,
// beside the storage file the two of them describe.
func foreignStore(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "mdbox")
	inbox := filepath.Join(root, "mailboxes", "INBOX", "dbox-Mails")
	storage := filepath.Join(root, "storage")
	for _, d := range []string{inbox, storage} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, b []byte) {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(inbox, "dovecot.index"), dboxref.IndexBase(t))
	write(filepath.Join(inbox, "dovecot.index.log"), dboxref.IndexLog(t))
	write(filepath.Join(inbox, "dovecot.index.log.2"), dboxref.IndexLogRotated(t))
	write(filepath.Join(storage, "dovecot.map.index.log"), dboxref.MapLog(t))
	write(filepath.Join(storage, "m.1"), dboxref.StoreFile(t))
	return home
}

func openStore(t *testing.T, home string) (mailbox.UserIndex, mailbox.UserMailbox) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u1@d00001.test", Home: home, Driver: "mdbox"}
	idx := indexfile.New().OpenUser(info)
	box := mdbox.New().OpenUser(info)
	t.Cleanup(func() { _ = idx.Close(); _ = box.Close() })
	return idx, box
}

// A store another implementation wrote is opened by our server and read as our
// own: their index becomes ours in place, the messages are not moved, and their
// index files are gone afterwards.
//
// The oracle is that implementation's own fetch over this store, recorded in
// the fixture README: uid 1 \Seen, uid 2 \Answered, uid 3 $Important, uid 4
// expunged, uid 5 nothing.
func TestAForeignStoreIsConvertedOnFirstOpen(t *testing.T) {
	home := foreignStore(t)
	idx, box := openStore(t, home)

	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open INBOX: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	// Four, not five: the expunged one is expunged, and a reader that replays
	// their appends without their expunges brings it back.
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, and their fetch reports four live", len(msgs))
	}

	want := []struct {
		flags   []string
		keyword string
	}{
		{[]string{`\Seen`}, ""},
		{nil, ""},
		{nil, "$Important"},
		{nil, ""},
	}
	for i, w := range want {
		got := msgs[i]
		if len(w.flags) == 1 && !hasFlag(got.Flags, w.flags[0]) {
			t.Errorf("message %d has flags %v, want %s", i+1, got.Flags, w.flags[0])
		}
		if w.keyword != "" && !hasFlag(got.Keywords, w.keyword) {
			t.Errorf("message %d has keywords %v, want %s", i+1, got.Keywords, w.keyword)
		}
	}

	// The bodies are reachable through our map, which is the whole claim of an
	// in-place conversion: the map entries point at their storage file, at their
	// offsets, and nothing was copied.
	for _, m := range msgs {
		rc, err := box.Fetch("INBOX", m.Filename, false)
		if err != nil {
			t.Fatalf("fetch %s: %v", m.Filename, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", m.Filename, err)
		}
		if len(b) == 0 {
			t.Errorf("message %s read as empty", m.Filename)
		}
	}

	// Theirs is gone, and only theirs.
	dir := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")
	for _, name := range []string{"dovecot.index", "dovecot.index.log", "dovecot.index.log.2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there after the conversion", name)
		}
	}
	// This store held one folder, so converting it converted the store: their
	// map has nothing left to serve and goes with it (#1569).
	if _, err := os.Stat(filepath.Join(home, "mdbox", "storage", "dovecot.map.index.log")); !os.IsNotExist(err) {
		t.Errorf("their map log outlived the last folder of theirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "mdbox", "storage", "m.1")); err != nil {
		t.Errorf("the storage file was touched: %v", err)
	}
}

// The second open is an ordinary open: there is nothing foreign left to find,
// and the state read back is the state the conversion wrote.
func TestASecondOpenDoesNotConvertAgain(t *testing.T) {
	home := foreignStore(t)

	idx, _ := openStore(t, home)
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	first, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()

	idx2, _ := openStore(t, home)
	f2, err := idx2.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	second, err := idx2.GetMessages(f2.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Fatalf("second open sees %d messages, first saw %d", len(second), len(first))
	}
	for i := range first {
		if first[i].UID != second[i].UID || first[i].Filename != second[i].Filename {
			t.Errorf("message %d reads as uid %d/%s, was uid %d/%s",
				i, second[i].UID, second[i].Filename, first[i].UID, first[i].Filename)
		}
	}
}

func hasFlag(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Their index files are not taken into our namespace on the strength of their
// name. The legacy canonical names are ours historically and theirs currently,
// and renaming theirs leaves their store unopenable by them and unreadable by
// us -- the file is gone from where they look and does not parse where we put
// it (#1574).
func TestAForeignIndexIsNotRenamedIntoOurNamespace(t *testing.T) {
	home := foreignStore(t)
	dir := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")

	// Read through the conversion path, which is what should claim this folder.
	idx, _ := openStore(t, home)
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open: %v", err)
	}

	// Whatever happened, it was not a rename: nothing of theirs may end up
	// under our name carrying their bytes.
	if b, err := os.ReadFile(filepath.Join(dir, "yarilo.index")); err == nil {
		if len(b) == len(dboxref.IndexBase(t)) {
			t.Error("their index was renamed to ours rather than converted")
		}
	}
}

// The same folder, with the conversion out of the picture: a foreign index must
// survive an open that has nothing to do with it.
func TestAForeignIndexSurvivesAnOpenThatCannotConvertIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dovecot.index"), dboxref.IndexBase(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dovecot.index.log"), dboxref.IndexLog(t), 0o600); err != nil {
		t.Fatal(err)
	}

	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@d00001.test", Home: dir})
	defer idx.Close() //nolint:errcheck
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open: %v", err)
	}

	for _, name := range []string{"dovecot.index", "dovecot.index.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was taken away by an open that does not understand it: %v", name, err)
		}
	}
}

// Theirs is removed only after ours is written. A flush that cannot happen must
// leave their index where it is: the alternative is the one state the design
// forbids, a folder with neither index (#1524).
func TestTheirIndexSurvivesAConversionThatCannotWriteOurs(t *testing.T) {
	home := foreignStore(t)
	dir := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")
	// Our index lands beside their files here, so a directory that refuses new
	// files is what a failed write looks like.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	idx, _ := openStore(t, home)
	if _, err := idx.OpenFolder("INBOX", 0); err == nil {
		t.Error("a folder whose index could not be written opened clean")
	}
	for _, name := range []string{"dovecot.index", "dovecot.index.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed although ours was never written: %v", name, err)
		}
	}
}

// The conversion keeps their UID space whole: the same UIDs, the same
// UIDVALIDITY, and their next_uid.
//
// This is what makes reading a store in place worth doing. A client reconnects
// over the same mailbox, finds the UIDs it left and resynchronises nothing; new
// numbers would make it refetch every message, which costs what a migration
// over IMAP costs. RFC 3501 puts it the other way round -- UIDVALIDITY changes
// when UIDs are not preserved -- so preserving them means it must not (#1568).
//
// The oracle is their own index header, read straight from the fixture rather
// than from anything of ours.
func TestTheConversionKeepsTheirUIDSpace(t *testing.T) {
	theirs, err := dboxindex.ParseHeader(dboxref.IndexBase(t))
	if err != nil {
		t.Fatal(err)
	}

	home := foreignStore(t)
	idx, _ := openStore(t, home)
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if f.UIDValidity != theirs.UIDValidity {
		t.Errorf("UIDVALIDITY is %d, and their index says %d", f.UIDValidity, theirs.UIDValidity)
	}

	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	// Their live UIDs: the base holds 1, 2, 3 and 5, and the log appends 6 and
	// expunges 5.
	want := []uint32{1, 2, 3, 6}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d", len(msgs), len(want))
	}
	for i, uid := range want {
		if msgs[i].UID != uid {
			t.Errorf("message %d has uid %d, want %d", i+1, msgs[i].UID, uid)
		}
	}

	// next_uid is 7 because uid 6 exists. The case this guards is the one their
	// counter records and their records do not: uid 4 and uid 5 are gone, so a
	// reader deriving the counter from what survives has no way to know how far
	// it really moved. Here that reader would agree by luck, because the
	// highest surviving uid happens to be the highest ever used.
	const wantNext = 7
	if f.NextUID != wantNext {
		t.Errorf("next uid is %d, want %d -- the highest surviving uid plus one is %d",
			f.NextUID, wantNext, want[len(want)-1]+1)
	}
}

// A folder whose last message was expunged keeps their next_uid, which is
// higher than anything the folder still holds.
//
// This is the case the counter records and the messages cannot: the reference
// moves next_uid as it applies each append and journals nothing, so a message
// appended and then expunged leaves the counter moved and no record behind. A
// reader that took the highest surviving uid plus one would hand the next
// delivery uid 3 -- a number a client has already seen carrying different mail
// (#1568).
//
// The oracle is the reference's own status output over the folder this fixture
// came from: messages=2 uidnext=4 uidvalidity=1788118011.
func TestAnExpungedTailKeepsTheirNextUID(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "mdbox")
	dir := filepath.Join(root, "mailboxes", "Tail", "dbox-Mails")
	storage := filepath.Join(root, "storage")
	for _, d := range []string{dir, storage} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, b := range map[string][]byte{
		filepath.Join(dir, "dovecot.index.log"):         dboxref.IndexTailLog(t),
		filepath.Join(storage, "dovecot.map.index.log"): dboxref.MapTailLog(t),
		filepath.Join(storage, "m.1"):                   dboxref.StoreTailFile(t),
	} {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	idx, _ := openStore(t, home)
	f, err := idx.OpenFolder("Tail", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const (
		wantUIDValidity = 1788118011
		wantNextUID     = 4
		wantMessages    = 2
	)
	if f.UIDValidity != wantUIDValidity {
		t.Errorf("UIDVALIDITY is %d, and the reference reports %d", f.UIDValidity, wantUIDValidity)
	}
	if f.NextUID != wantNextUID {
		t.Errorf("next uid is %d, and the reference reports %d; the highest surviving uid plus one is 3",
			f.NextUID, wantNextUID)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != wantMessages {
		t.Fatalf("got %d messages, and the reference reports %d", len(msgs), wantMessages)
	}
	if msgs[0].UID != 1 || msgs[1].UID != 2 {
		t.Errorf("uids are %d and %d, want 1 and 2", msgs[0].UID, msgs[1].UID)
	}
	if !hasFlag(msgs[0].Flags, `\Seen`) {
		t.Errorf("uid 1 has flags %v, and the reference reports \\Seen", msgs[0].Flags)
	}
	if len(msgs[1].Flags) != 0 {
		t.Errorf("uid 2 has flags %v, and the reference reports none", msgs[1].Flags)
	}
}

// Their map outlives a folder's conversion while another folder of theirs is
// still unconverted: that folder addresses its mail through their map uids, and
// removing the map would leave it unreadable by either implementation (#1569).
func TestTheirMapStaysWhileAFolderOfTheirsIsUnconverted(t *testing.T) {
	home := foreignStore(t)
	// A second folder of theirs, left unopened.
	second := filepath.Join(home, "mdbox", "mailboxes", "Archive", "dbox-Mails")
	if err := os.MkdirAll(second, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"dovecot.index":     dboxref.IndexBase(t),
		"dovecot.index.log": dboxref.IndexLog(t),
	} {
		if err := os.WriteFile(filepath.Join(second, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	idx, _ := openStore(t, home)
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open INBOX: %v", err)
	}
	mapLog := filepath.Join(home, "mdbox", "storage", "dovecot.map.index.log")
	if _, err := os.Stat(mapLog); err != nil {
		t.Fatalf("their map went with the first folder, and Archive still needs it: %v", err)
	}

	// Converting the last one ends the store's conversion.
	if _, err := idx.OpenFolder("Archive", 0); err != nil {
		t.Fatalf("open Archive: %v", err)
	}
	if _, err := os.Stat(mapLog); !os.IsNotExist(err) {
		t.Errorf("their map is still there after the last folder converted: %v", err)
	}
}

// A folder of theirs opened after their map is gone is refused, loudly. It
// cannot be converted -- the map is where its messages are addressed from -- and
// the alternative to refusing is an empty folder that looks healthy.
func TestAForeignFolderWithoutTheirMapIsRefused(t *testing.T) {
	home := foreignStore(t)
	if err := os.Remove(filepath.Join(home, "mdbox", "storage", "dovecot.map.index.log")); err != nil {
		t.Fatal(err)
	}
	idx, _ := openStore(t, home)
	if _, err := idx.OpenFolder("INBOX", 0); err == nil {
		t.Error("a foreign folder with no foreign map opened clean")
	}
}

// A store that cannot be written to is refused, and the refusal says what to do
// about it.
//
// The conversion ends by unlinking their index files, so on a read-only store
// it would fail on its last step having paid for every one before it -- and
// again on the next open. One loud answer instead (#1571).
func TestAReadOnlyStoreIsRefusedWithAWayOut(t *testing.T) {
	home := foreignStore(t)
	dir := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	idx, _ := openStore(t, home)
	_, err := idx.OpenFolder("INBOX", 0)
	if err == nil {
		t.Fatal("a read-only store converted")
	}
	if !errors.Is(err, dboxconv.ErrReadOnly) {
		t.Errorf("the refusal is %v, and it should say the store is read-only", err)
	}
	// An operator reading this has to learn the path and the alternative, not
	// just that something failed.
	for _, want := range []string{dir, "convert it offline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// Nothing of theirs was touched, and nothing of ours was left behind.
	for _, name := range []string{"dovecot.index", "dovecot.index.log", "dovecot.index.log.2"} {
		if _, serr := os.Stat(filepath.Join(dir, name)); serr != nil {
			t.Errorf("%s did not survive the refusal: %v", name, serr)
		}
	}

	// And it says the same thing next time: a folder that failed carries no
	// mark, because a mark is another silent state.
	if _, err2 := idx.OpenFolder("INBOX", 0); !errors.Is(err2, dboxconv.ErrReadOnly) {
		t.Errorf("the second open answers %v, and the mount has not changed", err2)
	}
}

// The probe runs before the work, not after it.
//
// Proved by giving the store two faults at once: unwritable, and no map of
// theirs. The map is what the conversion reads first, so whichever answer comes
// back names the check that ran first.
func TestTheWritabilityProbeRunsBeforeAnyWork(t *testing.T) {
	home := foreignStore(t)
	if err := os.Remove(filepath.Join(home, "mdbox", "storage", "dovecot.map.index.log")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	idx, _ := openStore(t, home)
	_, err := idx.OpenFolder("INBOX", 0)
	if !errors.Is(err, dboxconv.ErrReadOnly) {
		t.Errorf("the refusal is %v; the writability probe did not run first", err)
	}
}

// A mixed mount is refused too: the folder writable, storage/ not.
//
// The critical section writes to storage/ as well -- their map is imported into
// ours there, and removed from there when the last folder converts -- so probing
// only the folder's own directory would let the conversion get as far as writing
// our folder index and then stop, with their map neither imported nor removed
// (#1571).
func TestAStoreWhoseStorageIsReadOnlyIsRefused(t *testing.T) {
	home := foreignStore(t)
	storage := filepath.Join(home, "mdbox", "storage")
	if err := os.Chmod(storage, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(storage, 0o700) })

	idx, _ := openStore(t, home)
	_, err := idx.OpenFolder("INBOX", 0)
	if !errors.Is(err, dboxconv.ErrReadOnly) {
		t.Fatalf("the refusal is %v, and storage/ cannot be written to", err)
	}
	if !strings.Contains(err.Error(), storage) {
		t.Errorf("the refusal does not name %s: %v", storage, err)
	}

	// Nothing was written and nothing was taken: their folder index is intact
	// and ours was not left behind.
	dir := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")
	if _, serr := os.Stat(filepath.Join(dir, "dovecot.index")); serr != nil {
		t.Errorf("their index did not survive the refusal: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dir, "yarilo.index")); !os.IsNotExist(serr) {
		t.Errorf("our index was written although the conversion was refused: %v", serr)
	}
}

// A maildir folder does not keep another implementation's index files.
//
// A maildir is served from its own files -- the message is the file, the flags
// are in its name -- so their index says nothing this server needs. Left there
// it is a file a tool of theirs finds and reads as current. Their keyword file
// is deliberately not removed: it is the only record of what the letters in a
// filename mean (#1593, #1600).
func TestAMaildirFolderDropsTheirLeftoverIndexFiles(t *testing.T) {
	home := t.TempDir()
	their := map[string][]byte{
		"dovecot.index":       dboxref.IndexBase(t),
		"dovecot.index.log":   dboxref.IndexLog(t),
		"dovecot.index.log.2": dboxref.IndexLogRotated(t),
		"dovecot.index.cache": []byte("cache"),
		"dovecot-keywords":    []byte("0 $Important\n"),
	}
	for name, b := range their {
		if err := os.WriteFile(filepath.Join(home, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer idx.Close() //nolint:errcheck
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("open: %v", err)
	}

	for _, name := range []string{"dovecot.index", "dovecot.index.log", "dovecot.index.log.2", "dovecot.index.cache"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", name, err)
		}
	}
	// The one that carries meaning stays until we write one of our own.
	if _, err := os.Stat(filepath.Join(home, "dovecot-keywords")); err != nil {
		t.Errorf("their keyword file was removed, and nothing else names those letters: %v", err)
	}
	// Ours was written, and it is ours: theirs was not renamed into it.
	if b, err := os.ReadFile(filepath.Join(home, "yarilo.index")); err != nil {
		t.Errorf("no index of ours: %v", err)
	} else if len(b) == len(dboxref.IndexBase(t)) {
		t.Error("their index was renamed to ours rather than removed")
	}
}

// A dbox folder keeps them: the conversion reads them, and removing them early
// is removing the store.
func TestADboxFolderKeepsTheirIndexFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "mdbox", "mailboxes", "INBOX", "dbox-Mails")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"dovecot.index":     dboxref.IndexBase(t),
		"dovecot.index.log": dboxref.IndexLog(t),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx := indexfile.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"})
	defer idx.Close() //nolint:errcheck
	// No map of theirs, so the conversion refuses -- which is beside the point
	// here. What matters is that their files are still on disk afterwards.
	_, _ = idx.OpenFolder("INBOX", 1)
	for _, name := range []string{"dovecot.index", "dovecot.index.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed from a dbox folder, where the conversion reads it: %v", name, err)
		}
	}
}
