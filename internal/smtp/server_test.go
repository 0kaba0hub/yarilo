package smtp

import (
	"io"
	"net"
	"strconv"
	"testing"

	goSmtp "github.com/0kaba0hub/go-smtp"
	"github.com/emersion/go-sasl"

	"github.com/0kaba0hub/yarilo/internal/smtp/proxy"
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

// buildTestServer starts a submission SMTP server on a random port.
func buildTestServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	opts := Options{
		Config: config.SMTPProtocolConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		Auth: stubAuth{},
	}
	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeSubmit(ln, nil) }()
	return ln.Addr().String(), func() { ln.Close() }
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

// ---- Submission -------------------------------------------------------------

func TestSubmission_WrongPassword(t *testing.T) {
	addr, cleanup := buildTestServer(t)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	if err := c.Auth(plainAuth("alice@example.com", "wrong")); err == nil {
		t.Fatal("expected auth failure for wrong password")
	}
}

func TestSubmission_AuthOK(t *testing.T) {
	addr, cleanup := buildTestServer(t)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	if err := c.Auth(plainAuth("alice@example.com", "secret")); err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	// No relay configured → DATA must return 451.
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if err := c.Rcpt("bob@example.com", nil); err != nil {
		t.Fatalf("RCPT TO: %v", err)
	}
	wc, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	io.WriteString(wc, "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: hi\r\n\r\nHello\r\n") //nolint:errcheck
	if err := wc.Close(); err == nil {
		t.Fatal("expected 451 relay error when no relay configured")
	}
}

// stubRelayBackend is a minimal SMTP server used as a relay in tests.
type stubRelayBackend struct{}

func (b *stubRelayBackend) NewSession(_ *goSmtp.Conn) (goSmtp.Session, error) {
	return &stubRelaySession{}, nil
}

type stubRelaySession struct{}

func (s *stubRelaySession) Mail(_ string, _ *goSmtp.MailOptions) error { return nil }
func (s *stubRelaySession) Rcpt(_ string, _ *goSmtp.RcptOptions) error { return nil }
func (s *stubRelaySession) Data(r io.Reader) error                     { _, _ = io.ReadAll(r); return nil }
func (s *stubRelaySession) Reset()                                     {}
func (s *stubRelaySession) Logout() error                              { return nil }

func buildStubRelay(t *testing.T) config.SMTPRelayConfig {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := goSmtp.NewServer(&stubRelayBackend{})
	srv.Domain = "relay.test"
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { ln.Close() })

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return config.SMTPRelayConfig{Host: host, Port: port, SSL: "no", SSLVerify: false, ConnectTimeout: 5, CommandTimeout: 10}
}

func TestSubmission_Relay(t *testing.T) {
	relayCfg := buildStubRelay(t)
	opts := Options{
		Config: config.SMTPProtocolConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		Auth:  stubAuth{},
		Proxy: proxy.New(relayCfg, "mx.example.com"),
	}

	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeSubmit(ln, nil) }()
	t.Cleanup(func() { ln.Close() })

	c := dialSMTP(t, ln.Addr().String())
	defer c.Close()

	if err := c.Auth(plainAuth("alice@example.com", "secret")); err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	const body = "From: alice@example.com\r\nTo: bob@external.com\r\nSubject: relayed\r\n\r\nHello\r\n"
	sendMessage(t, c, "alice@example.com", "bob@external.com", body)
}

// ---- unit helpers -----------------------------------------------------------

func TestSession_Reset(t *testing.T) {
	s := &session{from: "a@b.com", rcpts: []string{"c@d.com"}}
	s.Reset()
	if s.from != "" || len(s.rcpts) != 0 {
		t.Error("Reset did not clear session state")
	}
}
