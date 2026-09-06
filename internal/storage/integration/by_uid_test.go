package integration_test

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A message is found from its uid alone, across a reopen: what the folder
// records is enough, and no sidecar takes part (#1700).
func TestAMessageIsFoundByItsUIDAfterAReopen(t *testing.T) {
	for _, tc := range []struct {
		driver string
		open   func(*mailbox.UserInfo) mailbox.UserMailbox
	}{
		{"sdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return dboxv2.New().OpenUser(i) }},
		{"mdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return mdbox.New().OpenUser(i) }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			home := t.TempDir()
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: tc.driver}
			box := tc.open(info)
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			idx := indexfile.New().OpenUser(info)
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			body := "From: a@b\r\n\r\n" + tc.driver + " body\r\n"
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Filename: saved, Size: uint32(len(body)), VSize: vsize, GUID: guid}
			if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", m); err != nil {
				t.Fatal(err)
			}
			uid := m.UID
			box.Close() //nolint:errcheck
			idx.Close() //nolint:errcheck

			// A fresh view of the same store, holding nothing from the save.
			box2 := tc.open(info)
			defer box2.Close() //nolint:errcheck
			addr, ok := mailbox.Driver(box2).(mailbox.UIDAddressable)
			if !ok {
				t.Fatalf("the %s driver cannot find a message by uid", tc.driver)
			}
			rc, err := addr.OpenByUID("INBOX", uid, false)
			if err != nil {
				t.Fatalf("open uid %d: %v", uid, err)
			}
			defer rc.Close() //nolint:errcheck
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Errorf("uid %d reads %q, want %q", uid, got, body)
			}
		})
	}
}

// Reading a message by uid costs the folder read and nothing else: no listing
// of the directory, and no walk of the map (#1700).
func TestReadingByUIDWalksNothing(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"}
	box := mdbox.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nx\r\n"
	var uids []uint32
	for i := 0; i < 5; i++ {
		saved, vsize, guid, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{Filename: saved, Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", m); err != nil {
			t.Fatal(err)
		}
		uids = append(uids, m.UID)
	}

	dirReads, mapWalks := mdbox.SetTestCounters()
	defer mdbox.SetTestCounters()
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		rc, oerr := mailbox.OpenMessage(box, "INBOX", m)
		if oerr != nil {
			t.Fatalf("uid %d: %v", m.UID, oerr)
		}
		rc.Close() //nolint:errcheck
	}
	if got := dirReads(); got != 0 {
		t.Errorf("reading %d messages listed the folder %d times", len(msgs), got)
	}
	if got := mapWalks(); got != 0 {
		t.Errorf("reading %d messages walked the map %d times", len(msgs), got)
	}
}

// A folder carrying a sidecar from an older build resolves without it: the
// sidecar is not consulted, so what it says cannot matter (#1700).
func TestAnOldSidecarIsIgnored(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	box := dboxv2.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nx\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Filename: saved, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", m); err != nil {
		t.Fatal(err)
	}

	// A sidecar an older build left, naming this uid something else entirely.
	sidecar := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", "yarilo.index.names")
	line := strconv.FormatUint(uint64(m.UID), 10) + "\tu.deadbeef\t9\n"
	if err := os.WriteFile(sidecar, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	fresh := indexfile.New().OpenUser(info)
	defer fresh.Close() //nolint:errcheck
	f2, err := fresh.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := fresh.GetMessages(f2.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	want := "u." + strconv.FormatUint(uint64(m.UID), 10)
	if msgs[0].Filename != want {
		t.Errorf("the record names %q, want %q: the sidecar was consulted", msgs[0].Filename, want)
	}
	rc, err := mailbox.OpenMessage(box, "INBOX", msgs[0])
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rc.Close() //nolint:errcheck
}
