package lmtplogin

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/loginproto"
	"github.com/0kaba0hub/yarilo/internal/warden"
	"github.com/0kaba0hub/yarilo/pkg/authtoken"
)

// ---- rcptUsername -----------------------------------------------------------

func TestRcptUsername(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"alice@example.com", "alice@example.com"},
		{"<alice@example.com>", "alice@example.com"},
		{" <alice@example.com> ", "alice@example.com"},
		{"alice+folder@example.com", "alice@example.com"},
		{"<alice+tag@example.com>", "alice@example.com"},
		{"<alice+a+b@example.com>", "alice@example.com"},
		{"alice@sub.example.com", "alice@sub.example.com"},
		// no @ → empty
		{"notauser", ""},
		{"", ""},
		// @ but empty local
		{"@example.com", "@example.com"},
	}
	for _, tc := range cases {
		got := rcptUsername(tc.in)
		if got != tc.want {
			t.Errorf("rcptUsername(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestResolveDirectorTag proves #746: a per-user director_tag userdb field
// overrides the session's static tag; a user with no field, or any lookup
// failure, falls back to "" so the caller uses the component's static
// DirectorTag instead.
func TestResolveDirectorTag(t *testing.T) {
	authAddr := startTestAuthWithUserdb(t, fakeUserdb{
		"alice@example.com": {Username: "alice@example.com", DirectorTag: "b"},
		"bob@example.com":   {Username: "bob@example.com"}, // no override
	})

	s := &session{opts: Options{AuthMasterAddr: authAddr}}

	if got := s.resolveDirectorTag("alice@example.com"); got != "b" {
		t.Errorf("alice: resolveDirectorTag = %q, want %q", got, "b")
	}
	if got := s.resolveDirectorTag("bob@example.com"); got != "" {
		t.Errorf("bob (no override): resolveDirectorTag = %q, want %q", got, "")
	}
	if got := s.resolveDirectorTag("carol@example.com"); got != "" {
		t.Errorf("carol (unknown user): resolveDirectorTag = %q, want %q", got, "")
	}

	// AuthMasterAddr unset — must not attempt a dial, just return "".
	s2 := &session{opts: Options{}}
	if got := s2.resolveDirectorTag("alice@example.com"); got != "" {
		t.Errorf("no AuthMasterAddr: resolveDirectorTag = %q, want %q", got, "")
	}
}

// ---- infrastructure ---------------------------------------------------------

// startTestWarden spins a real yarilo-warden on a random local port.
func startTestWarden(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("warden listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	srv := warden.NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	waitForTCP(t, addr)
	return addr
}

// startTestAuth spins a real yarilo-auth master server backed by an in-memory
// token store. SESSION commands succeed and return real one-time tokens.
func startTestAuth(t *testing.T) string {
	t.Helper()
	store := authtoken.New(30 * time.Second)
	srv := protocol.NewMasterServer(nil, protocol.WithMasterTokenStore(store))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("auth listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	waitForTCP(t, addr)
	return addr
}

// fakeUserdb is a minimal in-memory protocol.Userdb for tests that need
// userdb extra fields (e.g. director_tag, #746) on the USER response.
type fakeUserdb map[string]*protocol.UserInfo

func (f fakeUserdb) Lookup(username string) (*protocol.UserInfo, error) {
	return f[username], nil
}

// startTestAuthWithUserdb is startTestAuth with a userdb attached, so USER
// lookups (the master protocol's USER command, used by Client.Userdb) hit
// real data instead of always returning NOTFOUND.
func startTestAuthWithUserdb(t *testing.T, userdb protocol.Userdb) string {
	t.Helper()
	store := authtoken.New(30 * time.Second)
	srv := protocol.NewMasterServer(userdb, protocol.WithMasterTokenStore(store))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("auth listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	waitForTCP(t, addr)
	return addr
}

// waitForTCP retries until addr accepts a TCP connection or 2 s elapse.
func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", addr)
}

// startLMTPLogin creates and starts an lmtp-login server.
func startLMTPLogin(t *testing.T, opts Options) string {
	t.Helper()
	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("lmtplogin listen: %v", err)
	}
	addr := ln.Addr().String()
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	waitForTCP(t, addr)
	return addr
}

// ---- stub LMTP backend ------------------------------------------------------

// stubDelivery records one successful delivery accepted by the stub backend.
type stubDelivery struct {
	preamble loginproto.Preamble
	from     string
	rcpt     string
	body     string
}

// stubBackend is a minimal raw-TCP LMTP server that reads the YARILO preamble
// and then completes one LMTP transaction per connection.
type stubBackend struct {
	mu         sync.Mutex
	deliveries []stubDelivery
}

func newStubBackend(t *testing.T) (*stubBackend, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub backend listen: %v", err)
	}
	addr := ln.Addr().String()
	sb := &stubBackend{}
	go sb.serve(ln)
	t.Cleanup(func() { ln.Close() })
	return sb, addr
}

func (sb *stubBackend) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go sb.handleConn(conn)
	}
}

func (sb *stubBackend) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck

	rd := bufio.NewReader(conn)

	// The login proxy writes the preamble before reading the 220 greeting.
	rawLine, err := rd.ReadString('\n')
	if err != nil {
		return
	}
	pre, err := loginproto.ParseLine(strings.TrimRight(rawLine, "\n"))
	if err != nil {
		return
	}

	fmt.Fprintf(conn, "220 stub LMTP ready\r\n")

	cmd, err := rd.ReadString('\n')
	if err != nil || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(cmd)), "LHLO") {
		return
	}
	fmt.Fprintf(conn, "250 stub\r\n")

	cmd, err = rd.ReadString('\n')
	if err != nil || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(cmd)), "MAIL FROM") {
		return
	}
	from := extractPath(strings.TrimSpace(cmd)[10:])
	fmt.Fprintf(conn, "250 OK\r\n")

	cmd, err = rd.ReadString('\n')
	if err != nil || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(cmd)), "RCPT TO") {
		return
	}
	rcpt := extractPath(strings.TrimSpace(cmd)[8:])
	fmt.Fprintf(conn, "250 OK\r\n")

	cmd, err = rd.ReadString('\n')
	if err != nil || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(cmd)), "DATA") {
		return
	}
	fmt.Fprintf(conn, "354 Start\r\n")

	var body strings.Builder
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		if line == ".\r\n" {
			break
		}
		body.WriteString(line)
	}
	// LMTP: one 250 per RCPT TO
	fmt.Fprintf(conn, "250 OK\r\n")

	cmd, err = rd.ReadString('\n')
	if err == nil && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(cmd)), "QUIT") {
		fmt.Fprintf(conn, "221 Bye\r\n")
	}

	sb.mu.Lock()
	sb.deliveries = append(sb.deliveries, stubDelivery{
		preamble: pre,
		from:     from,
		rcpt:     rcpt,
		body:     body.String(),
	})
	sb.mu.Unlock()
}

func (sb *stubBackend) get(t *testing.T, timeout time.Duration) []stubDelivery {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sb.mu.Lock()
		d := append([]stubDelivery(nil), sb.deliveries...)
		sb.mu.Unlock()
		if len(d) > 0 {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// extractPath pulls the angle-bracket-enclosed address from ":<addr>" or "<addr>".
func extractPath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, ":")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "<>")
	return strings.TrimSpace(s)
}

// ---- raw MTA client ---------------------------------------------------------

type mtaConn struct {
	conn net.Conn
	rd   *bufio.Reader
}

func dialMTA(t *testing.T, addr string) *mtaConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial lmtplogin: %v", err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	t.Cleanup(func() { conn.Close() })
	return &mtaConn{conn: conn, rd: bufio.NewReader(conn)}
}

// readCode reads lines until the terminal line (4th char is space). Returns the
// response text. Fails the test if the final code differs from expect.
func (c *mtaConn) readCode(t *testing.T, expect int) string {
	t.Helper()
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) >= 4 && line[3] == ' ' {
			var code int
			fmt.Sscanf(line[:3], "%d", &code)
			if code != expect {
				t.Fatalf("want code %d, got %q", expect, line)
			}
			return line[4:]
		}
		// continuation line (e.g. "250-...) — keep reading
	}
}

// tryRcpt sends RCPT TO and returns the numeric response code without failing.
func (c *mtaConn) tryRcpt(t *testing.T, rcpt string) int {
	t.Helper()
	fmt.Fprintf(c.conn, "RCPT TO:<%s>\r\n", rcpt)
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read RCPT response: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) >= 4 && line[3] == ' ' {
			var code int
			fmt.Sscanf(line[:3], "%d", &code)
			return code
		}
	}
}

func (c *mtaConn) lmtpHandshake(t *testing.T) {
	t.Helper()
	c.readCode(t, 220) // banner
	fmt.Fprintf(c.conn, "LHLO smoketest\r\n")
	c.readCode(t, 250)
}

func (c *mtaConn) mailFrom(t *testing.T, addr string) {
	t.Helper()
	fmt.Fprintf(c.conn, "MAIL FROM:<%s>\r\n", addr)
	c.readCode(t, 250)
}

func (c *mtaConn) rcpt(t *testing.T, addr string) {
	t.Helper()
	code := c.tryRcpt(t, addr)
	if code != 250 {
		t.Fatalf("RCPT TO:<%s> returned %d, want 250", addr, code)
	}
}

func (c *mtaConn) data(t *testing.T, body string) {
	t.Helper()
	fmt.Fprintf(c.conn, "DATA\r\n")
	c.readCode(t, 354)
	fmt.Fprintf(c.conn, "%s\r\n.\r\n", body)
}

// readDataStatuses reads n per-recipient status lines after DATA dot.
func (c *mtaConn) readDataStatuses(t *testing.T, n int) []int {
	t.Helper()
	codes := make([]int, 0, n)
	for i := 0; i < n; i++ {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read data status %d: %v", i, err)
		}
		line = strings.TrimRight(line, "\r\n")
		var code int
		fmt.Sscanf(line[:3], "%d", &code)
		codes = append(codes, code)
	}
	return codes
}

func (c *mtaConn) quit(t *testing.T) {
	t.Helper()
	fmt.Fprintf(c.conn, "QUIT\r\n")
	c.readCode(t, 221)
}

// ---- tests ------------------------------------------------------------------

// TestServer_HappyPath runs one full LMTP delivery through lmtp-login,
// with a real warden, real auth master, and a stub backend.
// It verifies that the stub received the correct preamble fields.
func TestServer_HappyPath(t *testing.T) {
	wardenAddr := startTestWarden(t)
	authAddr := startTestAuth(t)
	stub, backendAddr := newStubBackend(t)

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:         "test.local",
		BackendAddr:      backendAddr,
		AuthMasterAddr:   authAddr,
		WardenAddr:       wardenAddr,
		ConcurrencyLimit: 5,
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, "alice@example.com")
	mta.data(t, "Subject: Test\r\n\r\nHello")
	codes := mta.readDataStatuses(t, 1)
	if codes[0] != 250 {
		t.Fatalf("data status = %d, want 250", codes[0])
	}
	mta.quit(t)

	deliveries := stub.get(t, 3*time.Second)
	if len(deliveries) != 1 {
		t.Fatalf("stub got %d deliveries, want 1", len(deliveries))
	}
	d := deliveries[0]
	if d.preamble.User != "alice@example.com" {
		t.Errorf("preamble USER = %q, want alice@example.com", d.preamble.User)
	}
	if d.preamble.Token == "" {
		t.Error("preamble TOKEN is empty")
	}
	if d.from != "sender@example.com" {
		t.Errorf("backend MAIL FROM = %q, want sender@example.com", d.from)
	}
	if d.rcpt != "alice@example.com" {
		t.Errorf("backend RCPT TO = %q, want alice@example.com", d.rcpt)
	}
}

// TestServer_PlusDetailStripped verifies that plus-extension in RCPT TO is
// stripped before reaching the backend preamble USER field.
func TestServer_PlusDetailStripped(t *testing.T) {
	authAddr := startTestAuth(t)
	stub, backendAddr := newStubBackend(t)

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:       "test.local",
		BackendAddr:    backendAddr,
		AuthMasterAddr: authAddr,
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, "alice+inbox@example.com")
	mta.data(t, "Subject: Plus\r\n\r\nBody")
	mta.readDataStatuses(t, 1)
	mta.quit(t)

	deliveries := stub.get(t, 3*time.Second)
	if len(deliveries) != 1 {
		t.Fatalf("stub got %d deliveries, want 1", len(deliveries))
	}
	if deliveries[0].preamble.User != "alice@example.com" {
		t.Errorf("preamble USER = %q, want alice@example.com", deliveries[0].preamble.User)
	}
}

// TestServer_MultiRecipient sends two RCPT TOs; verifies two separate backend
// connections, each with the correct preamble USER.
func TestServer_MultiRecipient(t *testing.T) {
	wardenAddr := startTestWarden(t)
	authAddr := startTestAuth(t)
	stub, backendAddr := newStubBackend(t)

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:         "test.local",
		BackendAddr:      backendAddr,
		AuthMasterAddr:   authAddr,
		WardenAddr:       wardenAddr,
		ConcurrencyLimit: 5,
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, "alice@example.com")
	mta.rcpt(t, "bob@example.com")
	mta.data(t, "Subject: Multi\r\n\r\nBody")
	codes := mta.readDataStatuses(t, 2)
	for i, code := range codes {
		if code != 250 {
			t.Errorf("data status[%d] = %d, want 250", i, code)
		}
	}
	mta.quit(t)

	// Wait for both backend connections to complete.
	var deliveries []stubDelivery
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stub.mu.Lock()
		deliveries = append([]stubDelivery(nil), stub.deliveries...)
		stub.mu.Unlock()
		if len(deliveries) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(deliveries) != 2 {
		t.Fatalf("stub got %d deliveries, want 2", len(deliveries))
	}

	users := make(map[string]bool)
	for _, d := range deliveries {
		users[d.preamble.User] = true
		if d.preamble.Token == "" {
			t.Errorf("preamble for %s has empty TOKEN", d.preamble.User)
		}
	}
	if !users["alice@example.com"] {
		t.Error("no delivery for alice@example.com")
	}
	if !users["bob@example.com"] {
		t.Error("no delivery for bob@example.com")
	}
}

// TestServer_WardenConcurrencyLimit verifies that RCPT TO returns 451 when the
// cluster-wide delivery count is already at the configured limit.
func TestServer_WardenConcurrencyLimit(t *testing.T) {
	wardenAddr := startTestWarden(t)
	authAddr := startTestAuth(t)
	_, backendAddr := newStubBackend(t)

	// Pre-fill 2 CONNECT slots via a direct warden client.
	sibling, err := warden.Dial(wardenAddr, nil, time.Second)
	if err != nil {
		t.Fatalf("sibling dial: %v", err)
	}
	defer sibling.Close()
	for _, id := range []string{"sib1", "sib2"} {
		if err := sibling.Connect(id, "charlie@example.com", "10.0.0.2", "lmtp"); err != nil {
			t.Fatalf("sibling connect %s: %v", id, err)
		}
	}

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:         "test.local",
		BackendAddr:      backendAddr,
		AuthMasterAddr:   authAddr,
		WardenAddr:       wardenAddr,
		ConcurrencyLimit: 2,
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")

	code := mta.tryRcpt(t, "charlie@example.com")
	if code != 451 {
		t.Errorf("RCPT TO at limit = %d, want 451", code)
	}
}

// TestServer_WardenUnavailable verifies that delivery proceeds (no hard 451)
// when warden is unreachable — the proxy warns and delivers without concurrency
// tracking.
func TestServer_WardenUnavailable(t *testing.T) {
	authAddr := startTestAuth(t)
	stub, backendAddr := newStubBackend(t)

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:       "test.local",
		BackendAddr:    backendAddr,
		AuthMasterAddr: authAddr,
		WardenAddr:     "127.0.0.1:1", // unreachable
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")

	// RCPT TO must succeed despite warden being down.
	code := mta.tryRcpt(t, "dave@example.com")
	if code != 250 {
		t.Errorf("RCPT TO with warden down = %d, want 250", code)
	}

	mta.data(t, "Subject: Resilience\r\n\r\nBody")
	codes := mta.readDataStatuses(t, 1)
	if codes[0] != 250 {
		t.Errorf("data status = %d, want 250", codes[0])
	}
	mta.quit(t)

	deliveries := stub.get(t, 3*time.Second)
	if len(deliveries) != 1 {
		t.Fatalf("stub got %d deliveries, want 1", len(deliveries))
	}
	if deliveries[0].preamble.User != "dave@example.com" {
		t.Errorf("preamble USER = %q, want dave@example.com", deliveries[0].preamble.User)
	}
}

// TestServer_AuthFail verifies that RCPT TO returns 451 when the auth master
// is unreachable.
func TestServer_AuthFail(t *testing.T) {
	_, backendAddr := newStubBackend(t)

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:       "test.local",
		BackendAddr:    backendAddr,
		AuthMasterAddr: "127.0.0.1:1", // unreachable
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")

	code := mta.tryRcpt(t, "eve@example.com")
	if code != 451 {
		t.Errorf("RCPT TO with auth down = %d, want 451", code)
	}
}

// TestServer_Reset verifies that RSET releases warden slots for all recipients
// accumulated since the last MAIL FROM.
func TestServer_Reset(t *testing.T) {
	wardenAddr := startTestWarden(t)
	authAddr := startTestAuth(t)
	_, backendAddr := newStubBackend(t)

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:         "test.local",
		BackendAddr:      backendAddr,
		AuthMasterAddr:   authAddr,
		WardenAddr:       wardenAddr,
		ConcurrencyLimit: 5,
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, "frank@example.com")

	// RSET — proxy must fire DISCONNECT
	fmt.Fprintf(mta.conn, "RSET\r\n")
	mta.readCode(t, 250)
	mta.quit(t)

	// Probe warden: count must be back to 0.
	probe, err := warden.Dial(wardenAddr, nil, time.Second)
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	defer probe.Close()
	count, err := probe.Lookup("frank@example.com", "lmtp")
	if err != nil {
		t.Fatalf("probe lookup: %v", err)
	}
	if count != 0 {
		t.Errorf("warden count after RSET = %d, want 0", count)
	}
}

// ---- #741: backend_addr/director_addr precedence + LOOKUP correlation id --

// startStubDirector starts a minimal raw-TCP director stub that performs the
// wire handshake, captures the first LOOKUP line it receives on capturedLine,
// and replies FAIL so the caller returns quickly.
func startStubDirector(t *testing.T, capturedLine *string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("director listen: %v", err)
	}
	addr := ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\n")
		fmt.Fprintf(conn, "DONE\n")
		rd := bufio.NewReader(conn)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "LOOKUP\t") {
				*capturedLine = line
				fields := strings.Split(line, "\t")
				id := ""
				if len(fields) > 1 {
					id = fields[1]
				}
				fmt.Fprintf(conn, "FAIL\t%s\treason=no-backends\n", id)
				return
			}
		}
	}()
	return addr
}

// TestResolveBackend_BackendAddrWinsPrecedence proves #741: when both
// BackendAddr and DirectorAddr are set, BackendAddr wins and the director is
// never even dialled (DirectorAddr points at an address nothing listens on —
// a dial attempt would surface as an error, but resolveBackend must not
// attempt one). This unifies lmtplogin with internal/login's existing
// precedence, which lmtplogin previously inverted.
func TestResolveBackend_BackendAddrWinsPrecedence(t *testing.T) {
	s := &session{opts: Options{
		BackendAddr:  "10.0.0.9:24",
		DirectorAddr: "127.0.0.1:1", // nothing listens here; a dial would fail
	}}
	addr, err := s.resolveBackend("user@example.com")
	if err != nil {
		t.Fatalf("resolveBackend: unexpected error (should not have dialled director): %v", err)
	}
	if addr != "10.0.0.9:24" {
		t.Errorf("resolveBackend = %q, want backend_addr %q", addr, "10.0.0.9:24")
	}
}

// TestDirectorLookup_NonEmptyCorrelationID proves #741: the LOOKUP line sent
// to the director carries a real, non-empty, monotonically distinct
// correlation id — previously always "".
func TestDirectorLookup_NonEmptyCorrelationID(t *testing.T) {
	var captured string
	directorAddr := startStubDirector(t, &captured)

	s := &session{opts: Options{DirectorAddr: directorAddr}}
	_, _ = s.directorLookup("user@example.com", "")

	if captured == "" {
		t.Fatal("director never received a LOOKUP line")
	}
	fields := strings.Split(captured, "\t")
	if len(fields) < 2 || fields[1] == "" {
		t.Fatalf("LOOKUP id field is empty: %q", captured)
	}
}
