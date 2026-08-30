package integration_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
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
