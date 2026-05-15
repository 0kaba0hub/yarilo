package director

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/cluster/ring"
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

	conn.Write([]byte("LOOKUP\t1\tuser@example.com\n"))
	line := readLine(t, sc)
	if !strings.HasPrefix(line, "FAIL\t1\t") {
		t.Errorf("expected FAIL, got %q", line)
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

	conn.Write([]byte("LOOKUP\t2\tuser@example.com\n"))
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

	conn.Write([]byte("LOOKUP\t3\talice@example.com\n"))
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

	conn.Write([]byte("LOOKUP\t4\tuser@example.com\n"))
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

	conn.Write([]byte("LOOKUP\t5\tuser@example.com\n"))
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

	conn.Write([]byte("LOOKUP\t6\tuser@example.com\n"))
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

func TestUserMove_OverridesRing(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.6\t10993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("USER-MOVE\talice@example.com\t10.0.0.99\t10993\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("USER-MOVE: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t7\talice@example.com\n"))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if len(parts) < 4 || parts[0] != "HOST" || parts[2] != "10.0.0.99" {
		t.Errorf("expected override HOST 10.0.0.99, got %q", line)
	}
}

func TestUserRelease_FallsBackToRing(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	conn.Write([]byte("BACKEND-UP\t10.0.0.7\t10993\timap\t100\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("USER-MOVE\tbob@example.com\t10.0.0.99\t10993\n"))
	readLine(t, sc) // OK

	conn.Write([]byte("USER-RELEASE\tbob@example.com\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("USER-RELEASE: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t8\tbob@example.com\n"))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if len(parts) < 4 || parts[0] != "HOST" || parts[2] == "10.0.0.99" {
		t.Errorf("expected ring backend after release, got %q", line)
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
	conn.Write([]byte("LOOKUP\t9\tcarol@example.com\n"))
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
	conn.Write([]byte("LOOKUP\t1\tuser@example.com\n"))
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
	c2.Write([]byte("LOOKUP\t10\tuser@example.com\n"))
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

var _ = fmt.Sprintf
