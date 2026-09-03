package jmap

import (
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A JMAP request holds its control-file locks under the one owner spelling.
//
// It passed the bare username: one segment, naming neither the process, nor the
// request, nor anything an operator reading held_by against a live IMAP session
// could act on. Six spellings were collapsed into one in #1647 and this was a
// seventh, still there because nothing refused it (#1670).
func TestAJMAPRequestNamesItself(t *testing.T) {
	dir := t.TempDir()
	lk := &testLocker{}
	st := &Storage{
		Mailbox: maildir.New(),
		Index:   fileindex.New(),
		ResolveUser: func(u string) (*mailbox.UserInfo, error) {
			return &mailbox.UserInfo{Username: u, Home: filepath.Join(dir, u), Driver: "maildir"}, nil
		},
		Locker: lk,
	}
	h, err := st.open("alice@test.com", "4hbQ6j4PCJz1RCkh1Fr")
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	// Subscriptions are a control file behind the lock, so a read takes it.
	if err := h.subs.Add("INBOX"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if lk.acquisitions() == 0 {
		t.Fatal("no lock was taken, so this measures nothing")
	}
	lk.mu.Lock()
	defer lk.mu.Unlock()
	for o := range lk.owners {
		if n := len(strings.Split(o, "/")); n != 4 {
			t.Errorf("a JMAP request announced itself as %q (%d segments, want 4)", o, n)
		}
		if !strings.HasSuffix(o, "/4hbQ6j4PCJz1RCkh1Fr") {
			t.Errorf("owner %q does not carry the request's session id", o)
		}
	}
}
