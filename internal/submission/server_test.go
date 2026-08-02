package submission

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/internal/submission/proxy"
	"github.com/yarilomail/yarilo/pkg/config"
)

// stubAuth accepts only alice@example.com / secret.
type stubAuth struct{}

func (stubAuth) AuthPlain(u, p string) error {
	if u == "alice@example.com" && p == "secret" {
		return nil
	}
	return goSmtp.ErrAuthFailed
}

// buildTestServer starts a submission server on a random port.
func buildTestServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	opts := Options{
		Config: config.SubmissionProtocolConfig{
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
	go func() { _ = srv.Serve(ln, nil) }()
	return ln.Addr().String(), func() { ln.Close() }
}

func dialSMTP(t *testing.T, addr string) *goSmtp.Client {
	t.Helper()
	c, err := goSmtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
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

// TestSubmission_EHLO_AdvertisesPlainAndLogin verifies the post-STARTTLS
// EHLO response lists both AUTH PLAIN and AUTH LOGIN, so legacy MUAs that
// only support LOGIN (older Outlook, some Android clients) can authenticate.
func TestSubmission_EHLO_AdvertisesPlainAndLogin(t *testing.T) {
	addr, cleanup := buildTestServer(t)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	ok, params := c.Extension("AUTH")
	if !ok {
		t.Fatal("AUTH extension not advertised")
	}
	if !strings.Contains(params, "PLAIN") {
		t.Errorf("AUTH PLAIN missing from EHLO: %q", params)
	}
	if !strings.Contains(params, "LOGIN") {
		t.Errorf("AUTH LOGIN missing from EHLO: %q", params)
	}
}

func TestSubmission_AuthLOGIN_OK(t *testing.T) {
	addr, cleanup := buildTestServer(t)
	defer cleanup()
	c := dialSMTP(t, addr)
	defer c.Close()

	if err := c.Auth(sasl.NewLoginClient("alice@example.com", "secret")); err != nil {
		t.Fatalf("AUTH LOGIN: %v", err)
	}
}

func TestSubmission_AuthLOGIN_Wrong(t *testing.T) {
	addr, cleanup := buildTestServer(t)
	defer cleanup()
	c := dialSMTP(t, addr)
	defer c.Close()

	if err := c.Auth(sasl.NewLoginClient("alice@example.com", "wrong")); err == nil {
		t.Fatal("expected AUTH LOGIN failure for wrong password")
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

func buildStubRelay(t *testing.T) config.RelayConfig {
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
	return config.RelayConfig{Host: host, Port: port, SSL: "no", SSLVerify: false, ConnectTimeout: 5, CommandTimeout: 10}
}

func TestSubmission_Relay(t *testing.T) {
	relayCfg := buildStubRelay(t)
	opts := Options{
		Config: config.SubmissionProtocolConfig{
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
	go func() { _ = srv.Serve(ln, nil) }()
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

// TestSubmission_STARTTLS_Advertised verifies that the submission server
// advertises STARTTLS when Options.TLSConfig is set, and accepts the
// STARTTLS command (not the broken pre-v0.3.6 behaviour where TLSConfig
// was never wired and STARTTLS returned 502).
func TestSubmission_STARTTLS_Advertised(t *testing.T) {
	cert, err := generateSelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Config: config.SubmissionProtocolConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		Auth:      stubAuth{},
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln, nil) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	if _, err := br.ReadString('\n'); err != nil { // 220 greeting
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "EHLO test\r\n"); err != nil {
		t.Fatal(err)
	}
	advertised := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "STARTTLS") {
			advertised = true
		}
		// last line has "250 " (space), not "250-"
		if len(line) >= 4 && line[3] == ' ' {
			break
		}
	}
	if !advertised {
		t.Fatal("STARTTLS not advertised in EHLO response")
	}

	if _, err := io.WriteString(conn, "STARTTLS\r\n"); err != nil {
		t.Fatal(err)
	}
	line, _ := br.ReadString('\n')
	if !strings.HasPrefix(line, "220") {
		t.Fatalf("expected 220 to STARTTLS, got: %q", line)
	}
}

func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// capturingRelayBackend records the last received body for assertions.
type capturingRelayBackend struct {
	mu   sync.Mutex
	body []byte
}

func (b *capturingRelayBackend) NewSession(_ *goSmtp.Conn) (goSmtp.Session, error) {
	return &capturingRelaySession{backend: b}, nil
}

type capturingRelaySession struct {
	backend *capturingRelayBackend
}

func (s *capturingRelaySession) Mail(_ string, _ *goSmtp.MailOptions) error { return nil }
func (s *capturingRelaySession) Rcpt(_ string, _ *goSmtp.RcptOptions) error { return nil }
func (s *capturingRelaySession) Data(r io.Reader) error {
	b, _ := io.ReadAll(r)
	s.backend.mu.Lock()
	s.backend.body = b
	s.backend.mu.Unlock()
	return nil
}
func (s *capturingRelaySession) Reset()        {}
func (s *capturingRelaySession) Logout() error { return nil }

func buildCapturingRelay(t *testing.T) (config.RelayConfig, *capturingRelayBackend) {
	t.Helper()
	be := &capturingRelayBackend{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := goSmtp.NewServer(be)
	srv.Domain = "relay.test"
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { ln.Close() })

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return config.RelayConfig{Host: host, Port: port, SSL: "no", SSLVerify: false, ConnectTimeout: 5, CommandTimeout: 10}, be
}

func runSubmissionWithReceivedHeader(t *testing.T, addReceived bool) []byte {
	t.Helper()
	relayCfg, captured := buildCapturingRelay(t)
	opts := Options{
		Config: config.SubmissionProtocolConfig{
			Hostname:          "mx.example.com",
			MaxMsgSize:        1 << 20,
			AddReceivedHeader: addReceived,
		},
		Auth:  stubAuth{},
		Proxy: proxy.New(relayCfg, "mx.example.com"),
	}
	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln, nil) }()
	t.Cleanup(func() { ln.Close() })

	c := dialSMTP(t, ln.Addr().String())
	defer c.Close()
	if err := c.Auth(plainAuth("alice@example.com", "secret")); err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	const body = "From: alice@example.com\r\nSubject: hi\r\n\r\nHi\r\n"
	sendMessage(t, c, "alice@example.com", "bob@external.com", body)
	captured.mu.Lock()
	defer captured.mu.Unlock()
	return captured.body
}

func TestSubmission_ReceivedHeader_AddedByDefault(t *testing.T) {
	got := runSubmissionWithReceivedHeader(t, true)
	if !bytes.HasPrefix(got, []byte("Received: from ")) {
		t.Fatalf("expected Received: header prepended, got body starting with:\n%q", string(got[:min(80, len(got))]))
	}
	if !bytes.Contains(got, []byte("by mx.example.com with ESMTPA")) {
		t.Fatalf("missing/wrong with-clause:\n%s", string(got))
	}
}

func TestSubmission_ReceivedHeader_Suppressed(t *testing.T) {
	got := runSubmissionWithReceivedHeader(t, false)
	if bytes.HasPrefix(got, []byte("Received:")) {
		t.Fatalf("expected no Received: header, got:\n%s", string(got))
	}
	if !bytes.HasPrefix(got, []byte("From: alice@example.com")) {
		t.Fatalf("expected body to start with original headers, got:\n%s", string(got))
	}
}
