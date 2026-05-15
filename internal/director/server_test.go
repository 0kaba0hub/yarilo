package director

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

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

func readHandshake(t *testing.T, sc *bufio.Scanner) {
	t.Helper()
	for sc.Scan() {
		if sc.Text() == "DONE" {
			return
		}
	}
	t.Fatal("server handshake never sent DONE")
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
	t.Helper()
	srv := New()
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

// --- individual test functions ---

func TestHandshake(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)
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
	// HOST\t{id}\t{ip}\t{port}\t{tag}
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

	conn.Write([]byte("LOOKUP\t3\tuser@example.com\n"))
	line := readLine(t, sc)
	if !strings.HasPrefix(line, "FAIL\t3\t") {
		t.Errorf("expected FAIL after backend removed, got %q", line)
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

	conn.Write([]byte("LOOKUP\t4\tuser@example.com\n"))
	line := readLine(t, sc)
	if !strings.HasPrefix(line, "FAIL\t4\t") {
		t.Errorf("expected FAIL after flush, got %q", line)
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

	// Client 1 registers a backend.
	c1, sc1 := dialTest(t, addr)
	readHandshake(t, sc1)
	sendHandshake(t, c1)

	// Client 2 connects and waits for push.
	c2, sc2 := dialTest(t, addr)
	readHandshake(t, sc2)
	sendHandshake(t, c2)

	// Client 1 sends BACKEND-UP — both should get RING-CHANGE.
	c1.Write([]byte("BACKEND-UP\t10.0.0.4\t10993\timap\t100\n"))

	// Client 1 gets OK response.
	if got := readLine(t, sc1); got != "OK" {
		t.Fatalf("c1 BACKEND-UP: expected OK, got %q", got)
	}

	// Client 2 gets unsolicited RING-CHANGE.
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
	readLine(t, sc) // OK (also RING-CHANGE for self — we don't care here)

	conn.Write([]byte("BACKEND-DOWN\t10.0.0.5\n"))
	// First line is the OK response.
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("BACKEND-DOWN: expected OK, got %q", got)
	}
}

func TestUserMove_OverridesRing(t *testing.T) {
	_, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	// Register a backend.
	conn.Write([]byte("BACKEND-UP\t10.0.0.6\t10993\timap\t100\n"))
	readLine(t, sc) // OK

	// Move alice to a different backend (not in ring).
	conn.Write([]byte("USER-MOVE\talice@example.com\t10.0.0.99\t10993\n"))
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("USER-MOVE: expected OK, got %q", got)
	}

	conn.Write([]byte("LOOKUP\t5\talice@example.com\n"))
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

	// After release, ring lookup should return the real backend.
	conn.Write([]byte("LOOKUP\t6\tbob@example.com\n"))
	line := readLine(t, sc)
	parts := strings.Split(line, "\t")
	if len(parts) < 4 || parts[0] != "HOST" || parts[2] == "10.0.0.99" {
		t.Errorf("expected ring backend after release, got %q", line)
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

func TestConfigurableVhosts(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	// Register backend with 50 vhosts.
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
	c2.Write([]byte("LOOKUP\t7\tuser@example.com\n"))
	line := readLine(t, sc2)
	if !strings.HasPrefix(line, "HOST\t7\t") {
		t.Errorf("expected HOST from shared ring, got %q", line)
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
