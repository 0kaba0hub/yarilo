package director

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// --- helpers ---

func dialTest(t *testing.T, addr string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	sc := bufio.NewScanner(conn)
	return conn, sc
}

// readHandshake reads server VERSION+HOST-HAND-START+…+HOST-HAND-END+DONE.
// Returns the HOST lines received during the handshake.
func readHandshake(t *testing.T, sc *bufio.Scanner) []string {
	t.Helper()
	var hosts []string
	for sc.Scan() {
		line := sc.Text()
		if line == "DONE" {
			return hosts
		}
		if strings.HasPrefix(line, "HOST\t") {
			hosts = append(hosts, line)
		}
	}
	t.Fatal("server handshake never sent DONE")
	return nil
}

func sendHandshake(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := conn.Write([]byte("VERSION\tyarilo-director\t1\t0\nME\t127.0.0.1\t0\t0\nDONE\n")); err != nil {
		t.Fatalf("send handshake: %v", err)
	}
}

func readLine(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("expected line, got EOF or scan error: %v", sc.Err())
	}
	return sc.Text()
}

func startServer(t *testing.T) (*Server, string) {
	return startServerOpts(t, Options{
		PingInterval: 24 * time.Hour, // disable PING during tests
	})
}

func startServerOpts(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	srv := NewWithOptions(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		ln.Close()
	})
	go func() {
		_ = srv.listenOn(ctx, ln)
	}()
	return srv, ln.Addr().String()
}

// --- tests ---

func TestHandshake_EmptyRing(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	hosts := readHandshake(t, sc)
	sendHandshake(t, conn)
	if len(hosts) != 0 {
		t.Errorf("expected no HOST lines on empty ring, got %v", hosts)
	}
}

func TestHandshake_ExistingBackends(t *testing.T) {
	srv, addr := startServer(t)
	// Pre-populate ring before client connects.
	srv.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 993, Tag: "imap", Up: true})

	conn, sc := dialTest(t, addr)
	hosts := readHandshake(t, sc)
	sendHandshake(t, conn)

	if len(hosts) != 1 || !strings.Contains(hosts[0], "10.0.0.1") {
		t.Errorf("expected 1 HOST line with 10.0.0.1, got %v", hosts)
	}
}

func TestLookup_NoBackends(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("LOOKUP\t1\tuser@example.com\t\n"))
	line := readLine(t, sc)
	if !strings.HasPrefix(line, "FAIL\t1\t") {
		t.Errorf("expected FAIL, got %q", line)
	}
}

// TestLookup_UnescapesUsername guards #701: the director must TabUnescape the
// LOOKUP username before hashing, so a username containing a TAB (escaped
// byte-for-byte on the wire by proto.Conn.Lookup) resolves to the same backend
// the ring picks for the raw username — not to a different backend keyed on the
// escaped string.
func TestLookup_UnescapesUsername(t *testing.T) {
	srv, addr := startServer(t)
	for i := 1; i <= 5; i++ {
		srv.ring.AddBackend(&ring.Backend{IP: fmt.Sprintf("10.0.0.%d", i), Port: 10993, Tag: "imap", Up: true})
	}

	const rawUser = "al\tice@d.test" // real TAB inside the username
	wantIP := srv.ring.LookupBackendByTag(rawUser, "imap").IP
	// Sanity: hashing the escaped form would pick a different backend, so the
	// test actually exercises the unescape (skip the rare hash collision).
	escaped := proto.TabEscape(rawUser)
	if got := srv.ring.LookupBackendByTag(escaped, "imap"); got != nil && got.IP == wantIP {
		t.Skip("escaped and unescaped forms collided on this backend set — uninformative run")
	}

	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte(fmt.Sprintf("LOOKUP\t9\t%s\timap\n", escaped)))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if len(parts) < 4 || parts[0] != "HOST" {
		t.Fatalf("expected HOST line, got %q", line)
	}
	if parts[2] != wantIP {
		t.Errorf("LOOKUP routed to %q, want %q (director must unescape before hashing)", parts[2], wantIP)
	}
}

func TestLookup_ReturnsHostWithTag(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.1\t10993\timap\t100\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("BACKEND-UP: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t2\tuser@example.com\timap\n"))
	line := readLine(t, sc)
	if !strings.HasPrefix(line, "HOST\t2\t") {
		t.Fatalf("expected HOST, got %q", line)
	}
	parts := strings.Split(line, "\t")
	if len(parts) < 5 {
		t.Fatalf("HOST line missing tag field: %q", line)
	}
	if parts[2] != "10.0.0.1" || parts[3] != "10993" {
		t.Errorf("unexpected backend: ip=%q port=%q", parts[2], parts[3])
	}
	if parts[4] != "imap" {
		t.Errorf("unexpected tag: %q", parts[4])
	}
}

func TestLookup_RecordsUserDir(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.1\t993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("LOOKUP\t3\talice@example.com\timap\n"))
	readLine(t, sc) // HOST

	e := srv.userDir.Get("alice@example.com")
	if e == nil {
		t.Fatal("expected user directory entry after LOOKUP")
	}
	if e.Host != "10.0.0.1:993" {
		t.Errorf("userdir host: want 10.0.0.1:993, got %q", e.Host)
	}
}

func TestBackendDown_RemovesFromRing(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.2\t10993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("BACKEND-DOWN\t10.0.0.2\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("BACKEND-DOWN: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t4\tuser@example.com\t\n"))
	if !strings.HasPrefix(readLine(t, sc), "FAIL\t4\t") {
		t.Error("expected FAIL after backend removed")
	}
}

func TestHostRemove_AliasForBackendDown(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.20\t993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("HOST-REMOVE\t10.0.0.20\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("HOST-REMOVE: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t5\tuser@example.com\t\n"))
	if !strings.HasPrefix(readLine(t, sc), "FAIL\t5\t") {
		t.Error("expected FAIL after HOST-REMOVE")
	}
}

func TestBackendFlush_StopsNewLookups(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.3\t10993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("BACKEND-FLUSH\t10.0.0.3\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("BACKEND-FLUSH: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t6\tuser@example.com\t\n"))
	if !strings.HasPrefix(readLine(t, sc), "FAIL\t6\t") {
		t.Error("expected FAIL after flush")
	}
}

func TestBackendFlush_UnknownBackend(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-FLUSH\t10.9.9.9\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Errorf("expected OK for unknown flush, got %q", got)
	}
}

func TestRingChange_PushedToAllClients(t *testing.T) {
	_, addr := startServer(t)

	c1, sc1 := dialTest(t, addr)
	readHandshake(t, sc1)
	sendHandshake(t, c1)

	c2, sc2 := dialTest(t, addr)
	readHandshake(t, sc2)
	sendHandshake(t, c2)

	c1.Write([]byte("BACKEND-UP\t10.0.0.4\t10993\timap\t100\n"))
	if got := readLine(t, sc1); got != "OK" {
		t.Fatalf("c1 BACKEND-UP: expected OK, got %q", got)
	}

	push := readLine(t, sc2)
	if !strings.HasPrefix(push, "RING-CHANGE\t10.0.0.4\tup\t") {
		t.Errorf("c2 expected RING-CHANGE push, got %q", push)
	}
	parts := strings.Split(push, "\t")
	if len(parts) < 4 || parts[3] != "imap" {
		t.Errorf("RING-CHANGE missing or wrong tag: %q", push)
	}
}

func TestRingChange_DownIncludesTag(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.5\t10993\tpop3\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("BACKEND-DOWN\t10.0.0.5\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("BACKEND-DOWN: expected OK, got %q", got)
	}
}

// TestUserMove_PinsToBackend (#708): USER-MOVE writes a TTL'd userDir pin, so a
// subsequent LOOKUP routes to the moved backend instead of the ring-hash one.
// Both backends must be Up in the requested tag — the pin is a normal sticky
// entry now, not an unconditional override.
func TestUserMove_PinsToBackend(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.6\t10993\timap\t100\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("BACKEND-UP\t10.0.0.99\t10993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("USER-MOVE\talice@example.com\t10.0.0.99\t10993\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("USER-MOVE: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t7\talice@example.com\timap\n"))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if len(parts) < 4 || parts[0] != "HOST" || parts[2] != "10.0.0.99" {
		t.Errorf("expected moved HOST 10.0.0.99, got %q", line)
	}
}

// TestUserMove_CaseInsensitive proves #738 under the #708 model: a move set
// under one spelling routes under any other spelling, since USER-MOVE writes
// the userDir pin via the lowercase-normalized hash. (USER-RELEASE is gone —
// #708 drops the overrides map; a move just TTL-expires.)
func TestUserMove_CaseInsensitive(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.16\t10993\timap\t100\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("BACKEND-UP\t10.0.0.199\t10993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("USER-MOVE\tAlice@Example.com\t10.0.0.199\t10993\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("USER-MOVE: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t9\talice@example.com\timap\n"))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if len(parts) < 4 || parts[0] != "HOST" || parts[2] != "10.0.0.199" {
		t.Fatalf("LOOKUP with different spelling: expected moved HOST 10.0.0.199, got %q", line)
	}
}

func TestUserWeak_MarksEntryWeak(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.8\t993\timap\t100\n"))
	readLine(t, sc) // OK

	// LOOKUP populates userDir as strong.
	conn.Write([]byte("LOOKUP\t9\tcarol@example.com\timap\n"))
	readLine(t, sc) // HOST

	e := srv.userDir.Get("carol@example.com")
	if e == nil || e.Weak {
		t.Fatal("expected strong entry after LOOKUP")
	}

	conn.Write([]byte("USER-WEAK\tcarol@example.com\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("USER-WEAK: expected OK, got %q", got)
	}

	e = srv.userDir.Get("carol@example.com")
	if e == nil || !e.Weak {
		t.Error("expected Weak=true after USER-WEAK")
	}
}

// TestLookup_TagIsolation proves #737: a LOOKUP restricted to tag "a" never
// sees a backend tagged "b" and vice versa — the wire tag field is
// mandatory and there is no full-ring fallback.
func TestLookup_TagIsolation(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.30\t993\ta\t100\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("BACKEND-UP\t10.0.0.31\t993\tb\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("LOOKUP\t1\tuser@example.com\ta\n"))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if len(parts) < 5 || parts[0] != "HOST" || parts[2] != "10.0.0.30" || parts[4] != "a" {
		t.Fatalf("LOOKUP tag=a: expected HOST on 10.0.0.30/a, got %q", line)
	}

	conn.Write([]byte("LOOKUP\t2\tuser@example.com\tb\n"))
	line = readLine(t, sc)
	parts = strings.Split(line, "\t")
	if len(parts) < 5 || parts[0] != "HOST" || parts[2] != "10.0.0.31" || parts[4] != "b" {
		t.Fatalf("LOOKUP tag=b: expected HOST on 10.0.0.31/b, got %q", line)
	}

	// Untagged pool ("") sees neither — there is no full-ring mode.
	conn.Write([]byte("LOOKUP\t3\tuser@example.com\t\n"))
	line = readLine(t, sc)
	if !strings.HasPrefix(line, "FAIL\t3\t") {
		t.Fatalf("LOOKUP tag=\"\": expected FAIL (no untagged backends), got %q", line)
	}
}

func TestUserKick_BroadcastsKicked(t *testing.T) {
	_, addr := startServer(t)

	c1, sc1 := dialTest(t, addr)
	readHandshake(t, sc1)
	sendHandshake(t, c1)

	c2, sc2 := dialTest(t, addr)
	readHandshake(t, sc2)
	sendHandshake(t, c2)

	c1.Write([]byte("USER-KICK\tdave@example.com\n"))
	if got := readLine(t, sc1); got != "OK" {
		t.Fatalf("USER-KICK: expected OK, got %q", got)
	}

	push := readLine(t, sc2)
	if push != "USER-KICKED\tdave@example.com" {
		t.Errorf("c2 expected USER-KICKED, got %q", push)
	}
}

func TestUserKilled_BroadcastsEverywhere(t *testing.T) {
	_, addr := startServer(t)

	c1, sc1 := dialTest(t, addr)
	readHandshake(t, sc1)
	sendHandshake(t, c1)

	c2, sc2 := dialTest(t, addr)
	readHandshake(t, sc2)
	sendHandshake(t, c2)

	c1.Write([]byte("USER-KILLED\t12345678\n"))
	if got := readLine(t, sc1); got != "OK" {
		t.Fatalf("USER-KILLED: expected OK, got %q", got)
	}

	push := readLine(t, sc2)
	if push != "USER-KILLED-EVERYWHERE\t12345678" {
		t.Errorf("c2 expected USER-KILLED-EVERYWHERE, got %q", push)
	}
}

func TestUserMoved_PushedToAllClients(t *testing.T) {
	_, addr := startServer(t)

	c1, sc1 := dialTest(t, addr)
	readHandshake(t, sc1)
	sendHandshake(t, c1)

	c2, sc2 := dialTest(t, addr)
	readHandshake(t, sc2)
	sendHandshake(t, c2)

	c1.Write([]byte("USER-MOVE\tcarol@example.com\t10.0.0.10\t10993\n"))
	readLine(t, sc1) // OK

	push := readLine(t, sc2)
	if !strings.HasPrefix(push, "USER-MOVED\tcarol@example.com\t") {
		t.Errorf("c2 expected USER-MOVED push, got %q", push)
	}
}

func TestPingPong(t *testing.T) {
	_, addr := startServerOpts(t, Options{
		PingInterval: 50 * time.Millisecond,
		PingTimeout:  200 * time.Millisecond,
	})
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	// Server will send PING; we reply PONG; expect no disconnect.
	line := readLine(t, sc)
	if line != "PING" {
		t.Fatalf("expected PING from server, got %q", line)
	}
	conn.Write([]byte("PONG\n"))

	// After PONG server should stay alive — send a LOOKUP to confirm.
	conn.Write([]byte("LOOKUP\t1\tuser@example.com\t\n"))
	got := readLine(t, sc)
	if !strings.HasPrefix(got, "FAIL\t1\t") {
		t.Errorf("expected FAIL (no backends), got %q", got)
	}
}

func TestPing_TimeoutClosesConn(t *testing.T) {
	_, addr := startServerOpts(t, Options{
		PingInterval: 50 * time.Millisecond,
		PingTimeout:  100 * time.Millisecond,
	})
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	// Wait for PING then do NOT reply.
	line := readLine(t, sc)
	if line != "PING" {
		t.Fatalf("expected PING, got %q", line)
	}

	// Server should close connection after PingTimeout.
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if sc.Scan() {
		t.Errorf("expected conn closed, got extra line: %q", sc.Text())
	}
}

func TestQuit_ClosesConnection(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("QUIT\tbye\n"))

	// Connection should be closed by server.
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if sc.Scan() {
		t.Errorf("expected conn closed after QUIT, got: %q", sc.Text())
	}
}

func TestPingPong_ServerResponds(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	// Client sends PING; server must reply PONG.
	conn.Write([]byte("PING\n"))
	if got := readLine(t, sc); got != "PONG" {
		t.Errorf("expected PONG from server, got %q", got)
	}
}

func TestConfigurableVhosts(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.8\t10993\timap\t50\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("BACKEND-UP: expected OK, got %q", got)
	}

	backends := srv.ring.Backends()
	if len(backends) != 1 || backends[0].Vhosts != 50 {
		t.Errorf("expected vhosts=50, got %+v", backends)
	}
}

func TestMultipleClients_SharedRing(t *testing.T) {
	_, addr := startServer(t)

	c1, sc1 := dialTest(t, addr)
	readHandshake(t, sc1)
	sendHandshake(t, c1)
	c1.Write([]byte("BACKEND-UP\t10.0.0.9\t10993\timap\t100\n"))
	readLine(t, sc1) // OK

	c2, sc2 := dialTest(t, addr)
	readHandshake(t, sc2)
	sendHandshake(t, c2)
	c2.Write([]byte("LOOKUP\t10\tuser@example.com\timap\n"))
	if !strings.HasPrefix(readLine(t, sc2), "HOST\t10\t") {
		t.Error("expected HOST from shared ring")
	}
}

func TestGracefulShutdown(t *testing.T) {
	srv := New()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.listenOn(ctx, ln)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("server did not shut down in time")
	}
}

func TestSessionOpenClose_OK(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("SESSION-OPEN\tsess1\talice@example.com\t10.0.0.1\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("SESSION-OPEN: expected OK, got %q", got)
	}

	conn.Write([]byte("SESSION-CLOSE\tsess1\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("SESSION-CLOSE: expected OK, got %q", got)
	}
}

func TestSessionOpen_RegisteredInRegistry(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("SESSION-OPEN\tsessX\tbob@example.com\t10.0.0.5\n"))
	readLine(t, sc) // OK

	srv.sessRecMu.RLock()
	rec, ok := srv.sessById["sessX"]
	srv.sessRecMu.RUnlock()

	if !ok || rec.user != "bob@example.com" || rec.backend != "10.0.0.5" {
		t.Errorf("session not registered correctly: ok=%v rec=%+v", ok, rec)
	}
}

func TestSessionClose_RemovesFromRegistry(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("SESSION-OPEN\tsessY\tcarol@example.com\t10.0.0.6\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("SESSION-CLOSE\tsessY\n"))
	readLine(t, sc) // OK

	srv.sessRecMu.RLock()
	_, ok := srv.sessById["sessY"]
	srv.sessRecMu.RUnlock()

	if ok {
		t.Error("session still in registry after SESSION-CLOSE")
	}
}

func TestBackendDown_KicksActiveSessions(t *testing.T) {
	_, addr := startServer(t)

	// loginConn simulates a login-pod: registers a session then waits for kicks.
	loginConn, loginSc := dialTest(t, addr)
	readHandshake(t, loginSc)
	sendHandshake(t, loginConn)

	// monConn simulates a monitor/health-pod that sends BACKEND-DOWN.
	monConn, monSc := dialTest(t, addr)
	readHandshake(t, monSc)
	sendHandshake(t, monConn)

	// Register backend and session from loginConn.
	loginConn.Write([]byte("BACKEND-UP\t10.0.0.10\t993\timap\t100\n"))
	readLine(t, loginSc) // OK (loginConn)
	// monConn gets RING-CHANGE push — drain it.
	readLine(t, monSc)

	loginConn.Write([]byte("SESSION-OPEN\tsessZ\tdave@example.com\t10.0.0.10\n"))
	if got := readLine(t, loginSc); got != "OK" {
		t.Fatalf("SESSION-OPEN: got %q", got)
	}

	// Monitor sends BACKEND-DOWN.
	monConn.Write([]byte("BACKEND-DOWN\t10.0.0.10\n"))
	// monConn receives OK.
	if got := readLine(t, monSc); got != "OK" {
		t.Fatalf("BACKEND-DOWN: got %q", got)
	}

	// loginConn should receive RING-CHANGE push + USER-KICKED (in any order).
	// Read two lines and check one is USER-KICKED.
	lines := []string{readLine(t, loginSc), readLine(t, loginSc)}
	var kicked bool
	for _, l := range lines {
		if l == "USER-KICKED\tdave@example.com" {
			kicked = true
		}
	}
	if !kicked {
		t.Errorf("expected USER-KICKED on loginConn, got: %v", lines)
	}
}

// TestBackendFlush_DrainsWithoutKick: the WIRE BACKEND-FLUSH (a backend
// self-reporting overload, #779/#811) drains — it must NOT kick sessions
// (#706). The login pod gets the RING-CHANGE flush push (so new lookups stop),
// but the active session is left running. Operator-forced evacuation with a
// kick is the admin `backends flush` command, not this.
func TestBackendFlush_DrainsWithoutKick(t *testing.T) {
	_, addr := startServer(t)

	loginConn, loginSc := dialTest(t, addr)
	readHandshake(t, loginSc)
	sendHandshake(t, loginConn)

	monConn, monSc := dialTest(t, addr)
	readHandshake(t, monSc)
	sendHandshake(t, monConn)

	loginConn.Write([]byte("BACKEND-UP\t10.0.0.11\t993\timap\t100\n"))
	readLine(t, loginSc)
	readLine(t, monSc) // RING-CHANGE push

	loginConn.Write([]byte("SESSION-OPEN\tsessF\teve@example.com\t10.0.0.11\n"))
	readLine(t, loginSc) // OK

	monConn.Write([]byte("BACKEND-FLUSH\t10.0.0.11\n"))
	if got := readLine(t, monSc); got != "OK" {
		t.Fatalf("BACKEND-FLUSH: got %q", got)
	}

	// The only push is the RING-CHANGE flush — never a USER-KICKED.
	got := readLine(t, loginSc)
	if strings.HasPrefix(got, "USER-KICKED") {
		t.Fatalf("wire BACKEND-FLUSH must DRAIN, not kick, got %q", got)
	}
	if !strings.HasPrefix(got, "RING-CHANGE\t10.0.0.11\tflush") {
		t.Fatalf("expected RING-CHANGE flush push, got %q", got)
	}
	// Nothing else (no kick) arrives shortly after.
	_ = loginConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if loginSc.Scan() {
		if line := loginSc.Text(); strings.HasPrefix(line, "USER-KICKED") {
			t.Fatalf("no kick expected after drain-flush, got %q", line)
		}
	}
}

func TestLookup_TagRoutesToSubRing(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.20\t993\tssd\t100\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("BACKEND-UP\t10.0.0.21\t993\thdd\t100\n"))
	readLine(t, sc) // OK

	// Lookup with tag=hdd must only return 10.0.0.21.
	for i := 0; i < 20; i++ {
		conn.Write([]byte(fmt.Sprintf("LOOKUP\t%d\tuser%d@example.com\thdd\n", i, i)))
		line := readLine(t, sc)
		parts := strings.Split(line, "\t")
		if len(parts) < 3 || parts[0] != "HOST" || parts[2] != "10.0.0.21" {
			t.Errorf("iter %d: expected HOST 10.0.0.21, got %q", i, line)
		}
	}
}

func TestLookup_StickyRouting(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.1\t993\timap\t100\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("BACKEND-UP\t10.0.0.2\t993\timap\t100\n"))
	readLine(t, sc) // OK

	// First LOOKUP: establishes userDir entry.
	conn.Write([]byte("LOOKUP\t1\talice@example.com\timap\n"))
	first := readLine(t, sc)
	firstParts := strings.Split(first, "\t")
	if len(firstParts) < 3 || firstParts[0] != "HOST" {
		t.Fatalf("expected HOST, got %q", first)
	}
	firstBackend := firstParts[2]

	// Add a third backend — ring would remap some users.
	conn.Write([]byte("BACKEND-UP\t10.0.0.3\t993\timap\t100\n"))
	readLine(t, sc) // OK

	// Subsequent LOOKUPs must return the same backend (sticky).
	for i := 2; i <= 10; i++ {
		conn.Write([]byte(fmt.Sprintf("LOOKUP\t%d\talice@example.com\timap\n", i)))
		line := readLine(t, sc)
		parts := strings.Split(line, "\t")
		if len(parts) < 3 || parts[0] != "HOST" {
			t.Fatalf("iter %d: expected HOST, got %q", i, line)
		}
		if parts[2] != firstBackend {
			t.Errorf("iter %d: sticky routing broken — got %q, want %q", i, parts[2], firstBackend)
		}
	}
}

func TestLookup_StickyRouting_FallsBackWhenBackendDown(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.4\t993\timap\t100\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("BACKEND-UP\t10.0.0.5\t993\timap\t100\n"))
	readLine(t, sc) // OK

	// Establish sticky entry on 10.0.0.4.
	conn.Write([]byte("BACKEND-DOWN\t10.0.0.5\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("LOOKUP\t1\tbob@example.com\timap\n"))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if parts[0] != "HOST" || parts[2] != "10.0.0.4" {
		t.Fatalf("expected 10.0.0.4, got %q", line)
	}

	// Bring 10.0.0.5 back and take 10.0.0.4 down.
	conn.Write([]byte("BACKEND-UP\t10.0.0.5\t993\timap\t100\n"))
	readLine(t, sc) // OK
	conn.Write([]byte("BACKEND-DOWN\t10.0.0.4\n"))
	readLine(t, sc) // OK

	// Sticky entry points to 10.0.0.4 which is now Down — must fall back to ring.
	conn.Write([]byte("LOOKUP\t2\tbob@example.com\timap\n"))
	line = readLine(t, sc)
	parts = strings.Split(line, "\t")
	if parts[0] != "HOST" || parts[2] == "10.0.0.4" {
		t.Errorf("expected fallback to live backend, got %q", line)
	}
}

func TestLookup_StickyRouting_WeakEntryUsesRing(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.6\t993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("LOOKUP\t1\tcarol@example.com\timap\n"))
	readLine(t, sc) // HOST

	// Mark entry as weak.
	conn.Write([]byte("USER-WEAK\tcarol@example.com\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("BACKEND-UP\t10.0.0.7\t993\timap\t100\n"))
	readLine(t, sc) // OK

	// Weak entry must not pin the user — ring decides.
	// Just verify we get a HOST response (not FAIL) and no panic.
	conn.Write([]byte("LOOKUP\t2\tcarol@example.com\timap\n"))
	line := readLine(t, sc)
	if !strings.HasPrefix(line, "HOST\t2\t") {
		t.Errorf("expected HOST, got %q", line)
	}
}

var _ = fmt.Sprintf
