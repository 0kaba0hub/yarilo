package integration_test

import (
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func TestDbgMdboxKey(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"}
	box := mdbox.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	f, _ := idx.OpenFolder("INBOX", 1)
	body := "x\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, 3, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("saved name = %q", saved)
	m := &mailbox.MessageMeta{Filename: saved, Size: 3, VSize: vsize, GUID: guid}
	if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", m); err != nil {
		t.Fatal(err)
	}
	t.Logf("after record: uid=%d mapuid=%d savedate=%d filename=%q", m.UID, m.MapUID, m.SaveDate, m.Filename)
	r := idx.(mailbox.StorageKeyReader)
	mu, sd, ok := r.StorageKey(f.ID, m.UID)
	t.Logf("same handle: mapuid=%d savedate=%d ok=%v", mu, sd, ok)
	idx.Close()
	idx2 := indexfile.New().OpenUser(info)
	f2, _ := idx2.OpenFolder("INBOX", 0)
	mu2, _, ok2 := idx2.(mailbox.StorageKeyReader).StorageKey(f2.ID, m.UID)
	t.Logf("fresh handle: mapuid=%d ok=%v", mu2, ok2)
}
