package imap_test

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	imapserver "github.com/0kaba0hub/yarilo/internal/imap"
	"github.com/0kaba0hub/yarilo/internal/quotawarn"
	"github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/mdbox"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
	"github.com/0kaba0hub/yarilo/pkg/quota"
)

// quotaAuthStub authenticates one user and returns a quota_rule so the session
// has a limit to cross.
type quotaAuthStub struct{ user, pass, rule string }

func (s *quotaAuthStub) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	if username == s.user && password == s.pass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username, QuotaRules: []string{s.rule}, IndexDir: "~/index"}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

func startQuotaWarnServer(t *testing.T, dir string, mb mailbox.MailboxBackend, withWarn bool) *imapclient.Client {
	t.Helper()
	opts := imapserver.Options{
		Mailbox:  mb,
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		Auth:     &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=1000"},
	}
	if withWarn {
		opts.QuotaPolicy = quota.Policy{
			Warnings: []quota.Warning{
				{Name: "under90", Resource: "storage", Threshold: "under", Percentage: 90},
			},
		}
		opts.QuotaWarner = quotawarn.New("", 5) // non-nil; Execute="" → only logs
	}
	srv := imapserver.New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

// TestQuotaWarnUnderDeleteOnly reproduces #570: a message delivered by one
// process (server A) then expunged by a fresh session (server B) that performed
// no save of its own. The "under" crossing must fire.
func TestQuotaWarnUnderDeleteOnly(t *testing.T) {
	for _, bk := range []struct {
		name string
		new  func() mailbox.MailboxBackend
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }},
	} {
		t.Run(bk.name, func(t *testing.T) { runDeleteOnlyUnder(t, bk.new) })
	}
}

func runDeleteOnlyUnder(t *testing.T, newMB func() mailbox.MailboxBackend) {
	dir := t.TempDir()

	// Server A — deliver a message well over the 1000-byte limit (as LMTP would).
	a := startQuotaWarnServer(t, dir, newMB(), false)
	big := "From: s@x\r\nTo: user@test.com\r\nSubject: big\r\n\r\n" + strings.Repeat("X", 4000) + "\r\n"
	ac := a.Append("INBOX", int64(len(big)), nil)
	if _, err := ac.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatal(err)
	}
	a.Logout().Wait() //nolint:errcheck

	// Capture slog to observe whether the under crossing fires.
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	// Server B — fresh session (separate index instance), delete-only expunge.
	b := startQuotaWarnServer(t, dir, newMB(), true)
	if _, err := b.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := b.Store(imap.SeqSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := b.Expunge().Close(); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	b.Logout().Wait() //nolint:errcheck

	logs := buf.String()
	if !strings.Contains(logs, "quota warning crossed") || !strings.Contains(logs, "threshold=under") {
		t.Fatalf("under crossing did not fire on delete-only session (#570).\nlogs:\n%s", logs)
	}
}
