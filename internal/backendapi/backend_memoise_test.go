package backendapi

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
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

// backend-API opens a user context per request; its bespoke driverCache is now
// the shared primitive. Two contexts on one driver must share the built instance,
// and the context path must be what uses it (#1149, #1155).
func TestBackendAPI_UserContextsShareTheMemoisedBackend(t *testing.T) {
	dir := t.TempDir()
	var builds, opens atomic.Int64

	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice@example.com": {Username: "alice@example.com", Home: filepath.Join(dir, "alice"), MailLocation: "mdbox:" + filepath.Join(dir, "alice", "mdbox")},
		"bob@example.com":   {Username: "bob@example.com", Home: filepath.Join(dir, "bob"), MailLocation: "mdbox:" + filepath.Join(dir, "bob", "mdbox")},
	}}
	srv := New(Options{
		Mailbox:  maildir.New(), // global default; the per-user driver differs
		Index:    fileindex.New(),
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%n"},
		MailboxByDriver: func(string) mailbox.MailboxBackend {
			builds.Add(1)
			return instrumentedBackend{inner: mdbox.New(), opens: &opens}
		},
		AuthClient: spawnAuthMaster(t, udb),
	})

	for _, u := range []string{"alice@example.com", "bob@example.com"} {
		uc, err := srv.openUserContext(u)
		if err != nil {
			t.Fatalf("openUserContext(%s): %v", u, err)
		}
		uc.Close()
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("backend built %d times across two user contexts, want 1 (#1149)", got)
	}
	if got := opens.Load(); got < 2 {
		t.Errorf("the memoised backend served %d opens, want >= 2: the context path did not go through it (#1155)", got)
	}
}
