package smtp

import (
	"io"
	"net"
	"strconv"
	"testing"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/0kaba0hub/yarilo/internal/lmtp"
	fileindex "github.com/0kaba0hub/yarilo/internal/storage/index/file"
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
		Config: config.SMTPProtocolConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
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

func (s *stubRelaySession) Mail(_ string, _ *goSmtp.MailOptions) error  { return nil }
func (s *stubRelaySession) Rcpt(_ string, _ *goSmtp.RcptOptions) error  { return nil }
func (s *stubRelaySession) Data(r io.Reader) error                       { _, _ = io.ReadAll(r); return nil }
func (s *stubRelaySession) Reset()                                       {}
func (s *stubRelaySession) Logout() error                                { return nil }

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
	dir := t.TempDir()
	mb, err := maildir.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx := fileindex.New(dir)
	defer idx.Close() //nolint:errcheck
	if err := mb.Init("alice@example.com"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	relayCfg := buildStubRelay(t)
	opts := Options{
		Config: config.SMTPProtocolConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		Auth:      stubAuth{},
		Deliverer: lmtp.New(mb, idx),
		Relay:     NewRelay(relayCfg),
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

func TestStripDelimiter(t *testing.T) {
	cases := []struct {
		addr  string
		delim string
		want  string
	}{
		{"user+tag@example.com", "+", "user@example.com"},
		{"user@example.com", "+", "user@example.com"},        // no tag
		{"user+tag@example.com", "", "user+tag@example.com"}, // delimiter disabled
		{"<user+tag@example.com>", "+", "user@example.com"},  // angle brackets stripped
		{"user+a+b@example.com", "+", "user@example.com"},    // multiple delimiters — strip at first
		{"nodomain", "+", "nodomain"},                        // no @ — passthrough
	}
	for _, tc := range cases {
		got := stripDelimiter(tc.addr, tc.delim)
		if got != tc.want {
			t.Errorf("stripDelimiter(%q, %q) = %q, want %q", tc.addr, tc.delim, got, tc.want)
		}
	}
}

