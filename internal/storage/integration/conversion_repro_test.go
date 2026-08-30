package integration_test

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func foreignHome(t *testing.T) string {
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
	for p, b := range map[string][]byte{
		filepath.Join(inbox, "dovecot.index"):           dboxref.IndexBase(t),
		filepath.Join(inbox, "dovecot.index.log"):       dboxref.IndexLog(t),
		filepath.Join(inbox, "dovecot.index.log.2"):     dboxref.IndexLogRotated(t),
		filepath.Join(storage, "dovecot.map.index.log"): dboxref.MapLog(t),
		filepath.Join(storage, "m.1"):                   dboxref.StoreFile(t),
	} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// A session opens the mailbox before it selects a folder, which is the order a
// real one has: the driver's map instance exists before the conversion runs.
func TestConversionBodiesReadableInTheSameSession(t *testing.T) {
	runConversionSession(t, "")
}

// The same, on a deployment that moves the index tree with INDEX=.
func TestConversionBodiesReadableWithASeparateIndexTree(t *testing.T) {
	runConversionSession(t, "%h/index")
}

func runConversionSession(t *testing.T, indexTmpl string) {
	t.Helper()
	dial := embeddedLocksForSaveTest(t)
	home := foreignHome(t)
	info := &mailbox.UserInfo{Username: "conv1@d00001.test", Home: home, Driver: "mdbox"}
	if indexTmpl != "" {
		info.IndexDir = mailbox.ExpandLocation(indexTmpl, home, info.Username)
	}

	box := mdbox.New(mdbox.WithLocker(dial())).OpenUser(info)
	defer box.Close() //nolint:errcheck
	// What a login does before any SELECT.
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := box.ListFolders(); err != nil {
		t.Fatalf("list: %v", err)
	}

	idx := indexfile.New(indexfile.WithLocker(dial())).OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages after conversion")
	}
	readAll(t, box, msgs)

	// And after a restart. A body that cannot be resolved is not only a failed
	// fetch: the folder is flagged for a rebuild, and the rebuild drops the
	// records that point nowhere -- so the same fault comes back the next time
	// as an empty mailbox rather than as an error (#1579).
	_ = idx.Close()
	_ = box.Close()

	box2 := mdbox.New(mdbox.WithLocker(dial())).OpenUser(info)
	defer box2.Close() //nolint:errcheck
	idx2 := indexfile.New(indexfile.WithLocker(dial())).OpenUser(info)
	defer idx2.Close() //nolint:errcheck

	f2, err := idx2.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open after restart: %v", err)
	}
	after, err := idx2.GetMessages(f2.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(msgs) {
		t.Fatalf("after a restart the folder holds %d messages, and it held %d", len(after), len(msgs))
	}
	if f2.UIDValidity != f.UIDValidity {
		t.Errorf("UIDVALIDITY is %d after a restart, was %d", f2.UIDValidity, f.UIDValidity)
	}
	readAll(t, box2, after)
}

func readAll(t *testing.T, box mailbox.UserMailbox, msgs []*mailbox.MessageMeta) {
	t.Helper()
	for _, m := range msgs {
		rc, err := box.Fetch("INBOX", m.Filename, false)
		if err != nil {
			t.Fatalf("uid %d (map uid %s): %v", m.UID, m.Filename, err)
		}
		b, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil || len(b) == 0 {
			t.Fatalf("uid %d read as %d bytes: %v", m.UID, len(b), rerr)
		}
	}
}

// Their subscriptions become ours, and land where a session reads them.
//
// This is the one part of a conversion a user sees the moment it happens: a
// folder list that changed under them. Their file lives with the mail, ours in
// the control root, and the two are different directories whenever a deployment
// sets one -- the same shape of fault as #1579, so it is tested with the two
// roots pulled apart (#1570).
func TestForeignSubscriptionsSurviveTheConversion(t *testing.T) {
	dial := embeddedLocksForSaveTest(t)
	home := foreignHome(t)
	control := filepath.Join(home, "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "mdbox", "subscriptions"),
		dboxref.Subscriptions(t), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second folder of theirs, left unopened, so the store's conversion is
	// not finished by the first one and their store-wide files are still due to
	// outlive it.
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

	info := &mailbox.UserInfo{
		Username:   "conv1@d00001.test",
		Home:       home,
		Driver:     "mdbox",
		ControlDir: control,
	}
	idx := indexfile.New(indexfile.WithLocker(dial())).OpenUser(info)
	defer idx.Close() //nolint:errcheck
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open: %v", err)
	}

	store := subs.New(mailbox.ControlRoot(info), "subscriptions", info.Username, "test", dial())
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got := make([]string, 0, len(snap))
	for name := range snap {
		got = append(got, name)
	}
	// The reference's own listing over the store this fixture came from.
	for _, want := range []string{"INBOX", "Archive", "Archive/2026", "Вхідні", "Вхідні/Робота"} {
		if !containsString(got, want) {
			t.Errorf("subscriptions are %v, and %q is missing", got, want)
		}
	}
	// Theirs stays while a folder of theirs is unconverted: store-wide state
	// goes with their map, on the last folder out, not with the first one in.
	theirFile := filepath.Join(home, "mdbox", "subscriptions")
	if _, err := os.Stat(theirFile); err != nil {
		t.Errorf("their subscription file went early: %v", err)
	}

	// Converting the last folder ends the store's conversion, and takes it.
	if _, err := idx.OpenFolder("Archive", 0); err != nil {
		t.Fatalf("open Archive: %v", err)
	}
	if _, err := os.Stat(theirFile); !os.IsNotExist(err) {
		t.Errorf("their subscription file outlived the last folder of theirs: %v", err)
	}
	// And ours still holds what theirs said.
	after, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(snap) {
		t.Errorf("subscriptions are %d after the store finished converting, were %d", len(after), len(snap))
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A converted store's map records carry the messages' GUIDs.
//
// Their map has no GUID field of its own, so the records an import appends have
// none, and the storage rebuild -- which pairs a physical record with its map
// entry by GUID -- would have nothing to match on a converted store. The value
// is not fetched from the storage files: their folder index carries one per
// message and the conversion has it in hand (#1573).
func TestConvertedMapRecordsCarryTheirGUIDs(t *testing.T) {
	dial := embeddedLocksForSaveTest(t)
	home := foreignHome(t)
	info := &mailbox.UserInfo{Username: "conv1@d00001.test", Home: home, Driver: "mdbox"}

	idx := indexfile.New(indexfile.WithLocker(dial())).OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}

	m, err := mdboxmap.Open(filepath.Join(home, "mdbox", "storage"), info.Username,
		mdboxmap.WithLocker(dial()))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close() //nolint:errcheck

	byUID := map[uint32]mdboxmap.MapEntry{}
	for _, r := range m.Records() {
		byUID[r.UID] = r
	}
	var zero [16]byte
	for _, msg := range msgs {
		mapUID, perr := strconv.ParseUint(msg.Filename, 10, 32)
		if perr != nil {
			t.Fatalf("uid %d: map uid %q: %v", msg.UID, msg.Filename, perr)
		}
		rec, ok := byUID[uint32(mapUID)]
		if !ok {
			t.Fatalf("uid %d names map uid %d, which the map does not carry", msg.UID, mapUID)
		}
		if msg.GUID == zero {
			t.Fatalf("uid %d has no GUID in the folder index, so this proves nothing", msg.UID)
		}
		if rec.GUID != msg.GUID {
			t.Errorf("map uid %d carries guid %x, and the message's is %x", mapUID, rec.GUID, msg.GUID)
		}
	}

	// The pairing the GUID exists for: a rebuild finds the record by it.
	first := msgs[0]
	if _, ok, lerr := m.LookupByGUID(first.GUID); lerr != nil || !ok {
		t.Errorf("looking a converted record up by its guid: ok=%v err=%v", ok, lerr)
	}
}

// Their store laid out the way their own INDEX= leaves it: the folder index and
// the map under the index root, only the message files with the mail.
//
// Looking for their files under the mail root alone finds nothing there, and
// nothing is exactly what a converted store looks like -- so the folder opens
// as new, with a fresh UIDVALIDITY and no messages, and not one line says why
// (#1583).
func TestAForeignStoreWithItsOwnIndexRootIsConverted(t *testing.T) {
	dial := embeddedLocksForSaveTest(t)
	home := t.TempDir()
	mail := filepath.Join(home, "mdbox")
	index := filepath.Join(home, "index")
	theirInbox := filepath.Join(index, "mailboxes", "INBOX", "dbox-Mails")
	for _, d := range []string{filepath.Join(mail, "storage"), filepath.Join(index, "storage"), theirInbox} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, b := range map[string][]byte{
		filepath.Join(theirInbox, "dovecot.index"):               dboxref.IndexBase(t),
		filepath.Join(theirInbox, "dovecot.index.log"):           dboxref.IndexLog(t),
		filepath.Join(theirInbox, "dovecot.index.log.2"):         dboxref.IndexLogRotated(t),
		filepath.Join(index, "storage", "dovecot.map.index.log"): dboxref.MapLog(t),
		filepath.Join(mail, "storage", "m.1"):                    dboxref.StoreFile(t),
	} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	info := &mailbox.UserInfo{
		Username: "conv3@d00001.test",
		Home:     home,
		Driver:   "mdbox",
		IndexDir: index,
	}
	box := mdbox.New(mdbox.WithLocker(dial())).OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := indexfile.New(indexfile.WithLocker(dial())).OpenUser(info)
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Their UIDVALIDITY, not a fresh one: a folder that opens as new is exactly
	// what the missed search produced.
	theirs, err := dboxindex.ParseHeader(dboxref.IndexBase(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.UIDValidity != theirs.UIDValidity {
		t.Errorf("UIDVALIDITY is %d, and their index says %d -- the folder opened as new",
			f.UIDValidity, theirs.UIDValidity)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages: their store was not found where their own INDEX= put it")
	}
	readAll(t, box, msgs)

	// Their files went from the index root, and the mail was not touched.
	for _, name := range []string{"dovecot.index", "dovecot.index.log"} {
		if _, serr := os.Stat(filepath.Join(theirInbox, name)); !os.IsNotExist(serr) {
			t.Errorf("%s is still there after the conversion", name)
		}
	}
	if _, serr := os.Stat(filepath.Join(mail, "storage", "m.1")); serr != nil {
		t.Errorf("the storage file was touched: %v", serr)
	}
}

// Their store with a separate index root and no dbox-Mails leaf: the folder's
// index sits in the mailbox directory itself.
//
// This is the field's layout, byte for byte from the report -- the local
// install writes the other one -- and it is not a variant we can choose between
// from here: the setting that decides it is theirs. Looked for in only one
// shape, a store in the other looks already converted, and every folder opens
// empty with a fresh UIDVALIDITY (#1583).
func TestAForeignStoreWithNoDboxMailsLeafIsConverted(t *testing.T) {
	dial := embeddedLocksForSaveTest(t)
	home := t.TempDir()
	mail := filepath.Join(home, "mdbox")
	index := filepath.Join(home, "index")
	// No dbox-Mails: the index files sit in the mailbox directory.
	theirInbox := filepath.Join(index, "mailboxes", "INBOX")
	for _, d := range []string{filepath.Join(mail, "storage"), filepath.Join(index, "storage"), theirInbox} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, b := range map[string][]byte{
		filepath.Join(theirInbox, "dovecot.index"):               dboxref.IndexBase(t),
		filepath.Join(theirInbox, "dovecot.index.log"):           dboxref.IndexLog(t),
		filepath.Join(theirInbox, "dovecot.index.log.2"):         dboxref.IndexLogRotated(t),
		filepath.Join(index, "storage", "dovecot.map.index.log"): dboxref.MapLog(t),
		filepath.Join(mail, "storage", "m.1"):                    dboxref.StoreFile(t),
	} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	info := &mailbox.UserInfo{
		Username: "conv4@d00001.test",
		Home:     home,
		Driver:   "mdbox",
		IndexDir: index,
	}
	box := mdbox.New(mdbox.WithLocker(dial())).OpenUser(info)
	defer box.Close() //nolint:errcheck
	idx := indexfile.New(indexfile.WithLocker(dial())).OpenUser(info)
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	theirs, err := dboxindex.ParseHeader(dboxref.IndexBase(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.UIDValidity != theirs.UIDValidity {
		t.Errorf("UIDVALIDITY is %d, and their index says %d -- the folder opened as new",
			f.UIDValidity, theirs.UIDValidity)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages: their store was not found in the flat layout")
	}
	readAll(t, box, msgs)

	// Theirs is gone from the mailbox directory, and what our own FTS or
	// anything else keeps beside it is not ours to remove.
	for _, name := range []string{"dovecot.index", "dovecot.index.log", "dovecot.index.log.2"} {
		if _, serr := os.Stat(filepath.Join(theirInbox, name)); !os.IsNotExist(serr) {
			t.Errorf("%s is still there after the conversion", name)
		}
	}
	// The store held one folder, so it is fully converted: their map goes too.
	if _, serr := os.Stat(filepath.Join(index, "storage", "dovecot.map.index.log")); !os.IsNotExist(serr) {
		t.Errorf("their map outlived the last folder of theirs: %v", serr)
	}
}

// In the flat layout, a folder of theirs that has not been converted still
// counts as one -- so their map stays.
//
// The walk that decides "is anything of theirs left" looked only at dbox-Mails
// leaves, and there are none here: it reported the store fully converted after
// the first folder and took their map away from every other one, which is the
// state that makes the rest of the store unreadable by either implementation
// (#1583, #1569).
func TestTheirMapStaysWhileAFlatLayoutFolderIsUnconverted(t *testing.T) {
	dial := embeddedLocksForSaveTest(t)
	home := t.TempDir()
	mail := filepath.Join(home, "mdbox")
	index := filepath.Join(home, "index")
	if err := os.MkdirAll(filepath.Join(mail, "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(index, "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"INBOX", "Archive"} {
		dir := filepath.Join(index, "mailboxes", folder)
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
	}
	for p, b := range map[string][]byte{
		filepath.Join(index, "storage", "dovecot.map.index.log"): dboxref.MapLog(t),
		filepath.Join(mail, "storage", "m.1"):                    dboxref.StoreFile(t),
	} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	info := &mailbox.UserInfo{
		Username: "conv5@d00001.test",
		Home:     home,
		Driver:   "mdbox",
		IndexDir: index,
	}
	idx := indexfile.New(indexfile.WithLocker(dial())).OpenUser(info)
	defer idx.Close() //nolint:errcheck
	if _, err := idx.OpenFolder("INBOX", 0); err != nil {
		t.Fatalf("open INBOX: %v", err)
	}
	mapLog := filepath.Join(index, "storage", "dovecot.map.index.log")
	if _, err := os.Stat(mapLog); err != nil {
		t.Fatalf("their map went with the first folder, and Archive still needs it: %v", err)
	}
	if _, err := idx.OpenFolder("Archive", 0); err != nil {
		t.Fatalf("open Archive: %v", err)
	}
	if _, err := os.Stat(mapLog); !os.IsNotExist(err) {
		t.Errorf("their map is still there after the last folder converted: %v", err)
	}
}
