package lmtp

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// instrumentedBackend records that IT served a delivery. Counting builds alone
// proves New wrapped the option; counting opens through the built instance
// proves the delivery path actually went through that instance -- so a call site
// that builds its own backend inline (the #1149 shape) fails here even though
// New still wraps the field.
type instrumentedBackend struct {
	inner mailbox.MailboxBackend
	opens *atomic.Int64
}

func (b instrumentedBackend) OpenUser(ui *mailbox.UserInfo) mailbox.UserMailbox {
	b.opens.Add(1)
	return b.inner.OpenUser(ui)
}

// The delivery path must obtain its backend from the memoised accessor: two
// deliveries on one driver build it once, and both are served by that instance.
// LMTP is the case that mattered -- the backend was selected once per recipient
// per message, so max_concurrent_writes never bounded delivery (#1149, #1155).
func TestLMTP_DeliveryUsesTheMemoisedBackend(t *testing.T) {
	dir := t.TempDir()
	var builds, opens atomic.Int64

	srv := New(Options{
		Hostname: "lmtp.test",
		Config: config.LMTPProtocolConfig{
			AddReceivedHeader: true,
			ReadTimeout:       5,
			WriteTimeout:      5,
		},
		Mailbox: maildir.New(), // global default; the per-user driver differs
		MailboxByDriver: func(string) mailbox.MailboxBackend {
			builds.Add(1)
			return instrumentedBackend{inner: mdbox.New(), opens: &opens}
		},
		Index:    fileindex.New(),
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		UserdbLookup: func(_ context.Context, user string) (*mailbox.UserInfo, error) {
			home := filepath.Join(dir, user)
			return &mailbox.UserInfo{
				Username: user, Home: home,
				MailPath: filepath.Join(home, "mdbox"), Driver: "mdbox",
			}, nil
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	conn, sc := dialLMTP(t, ln.Addr().String())
	sendLHLO(t, conn, sc)
	for i := 0; i < 2; i++ {
		resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", testMsg)
		if len(resp) == 0 || resp[0][0] != '2' {
			t.Fatalf("delivery %d not accepted: %v", i+1, resp)
		}
	}

	if got := builds.Load(); got != 1 {
		t.Errorf("backend built %d times across two deliveries, want 1: max_concurrent_writes bounds the process, not the message (#1149)", got)
	}
	if got := opens.Load(); got < 2 {
		t.Errorf("the memoised backend served %d opens, want >= 2: the delivery path did not go through it (#1155)", got)
	}
	fmt.Fprintf(conn, "QUIT\r\n")
}
