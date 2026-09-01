package dboxconv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const refData = "../dboxref/testdata"

// sdboxStore lays a folder out the way the reference wrote it: the index under
// its own root, the messages with the mail.
func sdboxStore(t *testing.T, log string, files ...string) (indexDir, mailDir string) {
	t.Helper()
	root := t.TempDir()
	indexDir = filepath.Join(root, "index", "mailboxes", "INBOX")
	mailDir = filepath.Join(root, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	for _, d := range []string{indexDir, mailDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyIn(t, filepath.Join(refData, log), filepath.Join(indexDir, "dovecot.index.log"))
	for _, f := range files {
		copyIn(t, filepath.Join(refData, f), filepath.Join(mailDir, strings.TrimPrefix(f, "sdbox-inbox-")))
	}
	return indexDir, mailDir
}

func copyIn(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read fixture %s: %v", from, err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A folder of theirs, converted, against what their own server reported for it.
//
// Everything here is the reference's own output, not a reading of the format:
// the uids, the flag and keyword sets, the guids and the uid space all come
// from doveadm run over the store these files were taken from.
func TestSdboxFolderConvertsToWhatTheirServerReported(t *testing.T) {
	indexDir, mailDir := sdboxStore(t, "sdbox-inbox.log",
		"sdbox-inbox-u.1", "sdbox-inbox-u.2", "sdbox-inbox-u.3", "sdbox-inbox-u.4")

	metas, hdr, missing, err := ConvertSdboxFolder(indexDir, mailDir)
	if err != nil {
		t.Fatal(err)
	}
	// doveadm mailbox status "uidnext messages uidvalidity" INBOX
	if hdr.UIDValidity != 1788252508 {
		t.Errorf("uidvalidity = %d, their server reported 1788252508", hdr.UIDValidity)
	}
	// uid 5 was expunged, so their next_uid is past a message that is gone:
	// 6 with four messages in the folder. Recomputing it from the survivors
	// would hand a client uid 5 again, carrying different mail (#1568).
	if hdr.NextUID != 6 {
		t.Errorf("next_uid = %d, their server reported 6", hdr.NextUID)
	}

	want := []struct {
		uid      uint32
		file     string
		flags    string
		keywords string
	}{
		{1, "u.1", `\Seen`, ""},
		{2, "u.2", `\Answered`, ""},
		{3, "u.3", "", "$Important"},
		{4, "u.4", "", "$Important $Label"},
	}
	if len(missing) != 0 {
		t.Errorf("uids reported missing = %v, and every file is in the folder", missing)
	}
	if len(metas) != len(want) {
		t.Fatalf("got %d messages, their server reported %d", len(metas), len(want))
	}
	for i, w := range want {
		m := metas[i]
		if m.UID != w.uid {
			t.Errorf("record %d: uid = %d, want %d", i, m.UID, w.uid)
		}
		if m.Filename != w.file {
			t.Errorf("uid %d: file = %q, want %q", w.uid, m.Filename, w.file)
		}
		if got := strings.Join(m.Flags, " "); got != w.flags {
			t.Errorf("uid %d: flags = %q, their server reported %q", w.uid, got, w.flags)
		}
		if got := strings.Join(m.Keywords, " "); got != w.keywords {
			t.Errorf("uid %d: keywords = %q, their server reported %q", w.uid, got, w.keywords)
		}
	}
}

// Their sdbox folder index carries no guid extension at all -- so there is no
// identity to be had here, and the conversion does not invent one. Where it
// does come from is the message file, read by the driver's own scan once the
// folder is marked guid-pending; that half is asserted where the folder is
// converted for real.
func TestTheirSdboxIndexCarriesNoGUID(t *testing.T) {
	indexDir, mailDir := sdboxStore(t, "sdbox-inbox.log",
		"sdbox-inbox-u.1", "sdbox-inbox-u.2", "sdbox-inbox-u.3", "sdbox-inbox-u.4")

	f, err := ReadForeignFolder(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Exts {
		if e.Name == "guid" {
			t.Fatalf("this fixture was taken from a store whose index has no guid extension; it now has one, so the test no longer covers what it says")
		}
	}

	metas, _, _, err := ConvertSdboxFolder(indexDir, mailDir)
	if err != nil {
		t.Fatal(err)
	}
	var zero [16]byte
	for _, m := range metas {
		if m.GUID != zero {
			t.Errorf("uid %d: the conversion produced a guid (%x) their index does not carry", m.UID, m.GUID)
		}
	}
}

// A record whose file is gone is left out rather than carried: a uid that
// serves nothing is worse than a uid that is not there, because a client keeps
// asking for it.
func TestSdboxRecordWithNoFileIsSkipped(t *testing.T) {
	indexDir, mailDir := sdboxStore(t, "sdbox-inbox.log",
		"sdbox-inbox-u.1", "sdbox-inbox-u.3", "sdbox-inbox-u.4")

	metas, _, missing, err := ConvertSdboxFolder(indexDir, mailDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 3 {
		t.Fatalf("got %d messages with u.2 removed, want 3", len(metas))
	}
	for _, m := range metas {
		if m.UID == 2 {
			t.Errorf("uid 2 was carried, and its file is not in the folder")
		}
	}
	// Skipping it quietly is the same healthy-looking emptiness this path
	// exists to avoid, one message at a time: the uid is reported so the
	// caller can say which message the folder lost.
	if len(missing) != 1 || missing[0] != 2 {
		t.Errorf("uids reported missing = %v, want [2]", missing)
	}
}
