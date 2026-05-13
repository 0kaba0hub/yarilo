package smtp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"testing"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/0kaba0hub/yarilo/internal/dkim"
	fileindex "github.com/0kaba0hub/yarilo/internal/storage/index/file"
	"github.com/0kaba0hub/yarilo/internal/lmtp"
	"github.com/0kaba0hub/yarilo/internal/storage/mailbox/maildir"
	"github.com/0kaba0hub/yarilo/pkg/config"
)

// stubAuth accepts only alice@example.com / secret.
type stubAuth struct{}

func (stubAuth) AuthPlain(u, p string) error {
	if u == "alice@example.com" && p == "secret" {
		return nil
	}
	return goSmtp.ErrAuthFailed
}

// buildTestServer starts an MX or submission SMTP server on a random port.
func buildTestServer(t *testing.T, submission bool) (addr string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New(dir)
	if err := mb.Init("alice@example.com"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	opts := Options{
		Config: config.SMTPConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		DKIMCfg:   config.DKIMConfig{},
		SPFCfg:    config.SPFConfig{Enabled: false},
		DMARCCfg:  config.DMARCConfig{Enabled: false},
		Auth:      stubAuth{},
		Deliverer: lmtp.New(mb, idx),
	}

	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if submission {
			_ = srv.ServeSubmit(ln, nil)
		} else {
			_ = srv.ServeMX(ln)
		}
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		idx.Close() //nolint:errcheck
	}
}

func dialSMTP(t *testing.T, addr string) *goSmtp.Client {
	t.Helper()
	c, err := goSmtp.Dial(addr)
	if err != nil {
		t.Fatalf("SMTP dial %s: %v", addr, err)
	}
	return c
}

func plainAuth(user, pass string) sasl.Client {
	return sasl.NewPlainClient("", user, pass)
}

func sendMessage(t *testing.T, c *goSmtp.Client, from, to, body string) {
	t.Helper()
	if err := c.Mail(from, nil); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if err := c.Rcpt(to, nil); err != nil {
		t.Fatalf("RCPT TO: %v", err)
	}
	wc, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := io.WriteString(wc, body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("end DATA: %v", err)
	}
}

// ---- MX inbound -------------------------------------------------------------

func TestMXInbound_Deliver(t *testing.T) {
	addr, cleanup := buildTestServer(t, false)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	const body = "From: sender@external.com\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nHello\r\n"
	sendMessage(t, c, "sender@external.com", "alice@example.com", body)
}

// ---- Submission -------------------------------------------------------------

func TestSubmission_WrongPassword(t *testing.T) {
	addr, cleanup := buildTestServer(t, true)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	if err := c.Auth(plainAuth("alice@example.com", "wrong")); err == nil {
		t.Fatal("expected auth failure for wrong password")
	}
}

func TestSubmission_AuthOK(t *testing.T) {
	addr, cleanup := buildTestServer(t, true)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	if err := c.Auth(plainAuth("alice@example.com", "secret")); err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	const body = "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: hi\r\n\r\nHello\r\n"
	sendMessage(t, c, "alice@example.com", "bob@example.com", body)
}

func TestSubmission_DKIMSign(t *testing.T) {
	dir := t.TempDir()
	mb, _ := maildir.New(dir)
	idx := fileindex.New(dir)
	defer idx.Close() //nolint:errcheck

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Config: config.SMTPConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		DKIMCfg: config.DKIMConfig{
			Sign:        true,
			Selector:    "sel",
			SignHeaders: []string{"From", "To", "Subject"},
		},
		SPFCfg:    config.SPFConfig{Enabled: false},
		DMARCCfg:  config.DMARCConfig{Enabled: false},
		Auth:      stubAuth{},
		KeyProv:   &staticKeyProvider{key: key},
		Deliverer: lmtp.New(mb, idx),
	}

	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeSubmit(ln, nil) }()
	defer ln.Close()

	c := dialSMTP(t, ln.Addr().String())
	defer c.Close()

	if err := c.Auth(plainAuth("alice@example.com", "secret")); err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	const body = "From: alice@example.com\r\nTo: bob@external.com\r\nSubject: signed\r\n\r\nSigned body\r\n"
	sendMessage(t, c, "alice@example.com", "bob@external.com", body)
}

// staticKeyProvider returns the same RSA key for any domain.
type staticKeyProvider struct{ key crypto.Signer }

func (p *staticKeyProvider) GetPrivateKey(_ context.Context, _ string) (crypto.Signer, error) {
	return p.key, nil
}

// ---- unit helpers -----------------------------------------------------------

func TestExtractDomain(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"user@example.com", "example.com"},
		{"<user@example.com>", "example.com"},
		{"nodomain", "nodomain"},
		{"", ""},
	}
	for _, tc := range cases {
		got := extractDomain(tc.addr)
		if got != tc.want {
			t.Errorf("extractDomain(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestParseMilterSocket(t *testing.T) {
	cases := []struct {
		socket   string
		wantNet  string
		wantAddr string
		wantErr  bool
	}{
		{"unix:/run/milter.sock", "unix", "/run/milter.sock", false},
		{"/run/milter.sock", "unix", "/run/milter.sock", false},
		{"tcp:127.0.0.1:7357", "tcp", "127.0.0.1:7357", false},
		{"udp:localhost:1234", "", "", true},
	}
	for _, tc := range cases {
		n, a, err := parseMilterSocket(tc.socket)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMilterSocket(%q): expected error", tc.socket)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMilterSocket(%q): %v", tc.socket, err)
			continue
		}
		if n != tc.wantNet || a != tc.wantAddr {
			t.Errorf("parseMilterSocket(%q) = (%q,%q), want (%q,%q)", tc.socket, n, a, tc.wantNet, tc.wantAddr)
		}
	}
}

func TestSession_Reset(t *testing.T) {
	s := &session{from: "a@b.com", rcpts: []string{"c@d.com"}}
	s.Reset()
	if s.from != "" || len(s.rcpts) != 0 {
		t.Error("Reset did not clear session state")
	}
}

func TestIsReject_Nil(t *testing.T) {
	if isReject(nil) {
		t.Error("nil action should not be a reject")
	}
}

// Compile-time check: dkim.KeyProvider is satisfied by staticKeyProvider.
var _ dkim.KeyProvider = (*staticKeyProvider)(nil)
