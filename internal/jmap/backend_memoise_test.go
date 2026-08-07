package jmap

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type instrumentedBackend struct {
	inner mailbox.MailboxBackend
	opens *atomic.Int64
}

func (b instrumentedBackend) OpenUser(ui *mailbox.UserInfo) mailbox.UserMailbox {
	b.opens.Add(1)
	return b.inner.OpenUser(ui)
}

// JMAP has no session: a store is opened per request, so without memoisation
// every request built a backend and its own write semaphore. Two opens on one
// driver must share the built instance, and the request path must be what uses
// it (#1149, #1155).
func TestJMAP_RequestsShareTheMemoisedBackend(t *testing.T) {
	dir := t.TempDir()
	var builds, opens atomic.Int64

	st := &Storage{
		Mailbox: maildir.New(), // global default; the per-user driver differs
		Index:   fileindex.New(),
		ResolveUser: func(u string) (*mailbox.UserInfo, error) {
			home := filepath.Join(dir, u)
			return &mailbox.UserInfo{
				Username: u, Home: home,
				MailPath: filepath.Join(home, "mdbox"), Driver: "mdbox",
			}, nil
		},
		MailboxByDriver: func(string) mailbox.MailboxBackend {
			builds.Add(1)
			return instrumentedBackend{inner: mdbox.New(), opens: &opens}
		},
		Locker: &testLocker{},
	}
	New(Options{Storage: st}) // wraps st.MailboxByDriver once

	for _, u := range []string{"alice@test.com", "bob@test.com"} {
		h, err := st.open(u)
		if err != nil {
			t.Fatalf("open(%s): %v", u, err)
		}
		h.close()
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("backend built %d times across two requests, want 1 (#1149)", got)
	}
	if got := opens.Load(); got < 2 {
		t.Errorf("the memoised backend served %d opens, want >= 2: the request path did not go through it (#1155)", got)
	}
}
