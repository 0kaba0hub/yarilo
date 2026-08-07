package pop3

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// instrumentedBackend records that IT served the session, so the test asserts
// the login path went through the memoised accessor -- not merely that New
// wrapped the field (#1155).
type instrumentedBackend struct {
	inner mailbox.MailboxBackend
	opens *atomic.Int64
}

func (b instrumentedBackend) OpenUser(ui *mailbox.UserInfo) mailbox.UserMailbox {
	b.opens.Add(1)
	return b.inner.OpenUser(ui)
}

// mailLocAuth authenticates and hands back a per-user mail_location, so the
// session stamps a driver and reaches the per-driver backend selection.
type mailLocAuth struct{ home string }

func (a *mailLocAuth) Authenticate(user, pass, _, _ string) (*protocol.AuthResponse, error) {
	if pass != "pw" {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	home := filepath.Join(a.home, user)
	return &protocol.AuthResponse{
		Result:   protocol.AuthOK,
		Username: user,
		Home:     home,
		MailLoc:  "mdbox:" + filepath.Join(home, "mdbox"),
	}, nil
}

// Two POP3 sessions on ONE server must share the per-driver backend: the
// selection happens per login, so before #1149 each connection built its own
// backend and its own write semaphore. Both sessions must be served by the one
// built instance (#1155).
func TestPOP3_SessionsShareTheMemoisedBackend(t *testing.T) {
	dir := t.TempDir()
	var builds, opens atomic.Int64

	srv := New(Options{
		Auth:     &mailLocAuth{home: dir},
		Mailbox:  maildir.New(), // global default; the per-user driver differs
		Index:    fileindex.New(),
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%n"},
		MailboxByDriver: func(string) mailbox.MailboxBackend {
			builds.Add(1)
			return instrumentedBackend{inner: mdbox.New(), opens: &opens}
		},
	})

	login := func(user string) {
		c, s := net.Pipe()
		deadline := time.Now().Add(5 * time.Second)
		c.SetDeadline(deadline) //nolint:errcheck
		s.SetDeadline(deadline) //nolint:errcheck
		go srv.newSession(s).serve()
		t.Cleanup(func() { c.Close() }) //nolint:errcheck

		br := bufio.NewReader(c)
		if _, err := br.ReadString('\n'); err != nil { // greeting
			t.Fatalf("greeting: %v", err)
		}
		send(t, c, "USER "+user)
		if resp := readline(t, br); !strings.HasPrefix(resp, "+OK") {
			t.Fatalf("USER: %q", resp)
		}
		send(t, c, "PASS pw")
		if resp := readline(t, br); !strings.HasPrefix(resp, "+OK") {
			t.Fatalf("PASS: %q", resp)
		}
		send(t, c, "QUIT")
		readline(t, br) //nolint:errcheck
	}

	login("alice@test.com")
	login("bob@test.com")

	if got := builds.Load(); got != 1 {
		t.Errorf("backend built %d times across two sessions, want 1: the write semaphore must be shared (#1149)", got)
	}
	if got := opens.Load(); got < 2 {
		t.Errorf("the memoised backend served %d opens, want >= 2: the login path did not go through it (#1155)", got)
	}
}
