package director

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func dial(t *testing.T, addr string) (net.Conn, *bufio.Scanner) {
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
	_, err := conn.Write([]byte("VERSION\tyarilo-director\t1\t0\nME\t127.0.0.1\t0\t0\nDONE\n"))
	if err != nil {
		t.Fatalf("send handshake: %v", err)
	}
}

func readLine(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("expected line, got EOF")
	}
	return sc.Text()
}

func startServer(t *testing.T) string {
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
	return ln.Addr().String()
}

var serverTests = []struct {
	name string
	fn   func(t *testing.T, addr string)
}{
	{
		name: "Handshake",
		fn: func(t *testing.T, addr string) {
			conn, sc := dial(t, addr)
			readHandshake(t, sc)
			sendHandshake(t, conn)
		},
	},
	{
		name: "Lookup_NoBackends",
		fn: func(t *testing.T, addr string) {
			conn, sc := dial(t, addr)
			readHandshake(t, sc)
			sendHandshake(t, conn)

			conn.Write([]byte("LOOKUP\t1\tuser@example.com\n"))
			line := readLine(t, sc)
			if !strings.HasPrefix(line, "FAIL\t1\t") {
				t.Errorf("expected FAIL, got %q", line)
			}
		},
	},
	{
		name: "BackendUp_Lookup_ReturnsHost",
		fn: func(t *testing.T, addr string) {
			conn, sc := dial(t, addr)
			readHandshake(t, sc)
			sendHandshake(t, conn)

			conn.Write([]byte("BACKEND-UP\t10.0.0.1\t10993\tpod-a\n"))
			if got := readLine(t, sc); got != "OK" {
				t.Fatalf("BACKEND-UP: expected OK, got %q", got)
			}

			conn.Write([]byte("LOOKUP\t2\tuser@example.com\n"))
			line := readLine(t, sc)
			if !strings.HasPrefix(line, "HOST\t2\t") {
				t.Errorf("expected HOST, got %q", line)
			}
			parts := strings.Split(line, "\t")
			if len(parts) < 4 {
				t.Fatalf("HOST line malformed: %q", line)
			}
			if parts[2] != "10.0.0.1" || parts[3] != "10993" {
				t.Errorf("unexpected backend: ip=%q port=%q", parts[2], parts[3])
			}
		},
	},
	{
		name: "BackendDown_RemovesFromRing",
		fn: func(t *testing.T, addr string) {
			conn, sc := dial(t, addr)
			readHandshake(t, sc)
			sendHandshake(t, conn)

			conn.Write([]byte("BACKEND-UP\t10.0.0.2\t10993\tpod-b\n"))
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
		},
	},
	{
		name: "MultipleClients_SharedRing",
		fn: func(t *testing.T, addr string) {
			// client 1 registers a backend
			c1, sc1 := dial(t, addr)
			readHandshake(t, sc1)
			sendHandshake(t, c1)
			c1.Write([]byte("BACKEND-UP\t10.0.0.3\t10993\tpod-c\n"))
			readLine(t, sc1) // OK

			// client 2 looks up — should see the backend registered by client 1
			c2, sc2 := dial(t, addr)
			readHandshake(t, sc2)
			sendHandshake(t, c2)
			c2.Write([]byte("LOOKUP\t4\tuser@example.com\n"))
			line := readLine(t, sc2)
			if !strings.HasPrefix(line, "HOST\t4\t") {
				t.Errorf("expected HOST from shared ring, got %q", line)
			}
		},
	},
	{
		name: "GracefulShutdown",
		fn: func(t *testing.T, addr string) {
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
		},
	},
}

func TestServer(t *testing.T) {
	// Each sub-test gets its own server to avoid shared ring state.
	for _, tc := range serverTests {
		t.Run(tc.name, func(t *testing.T) {
			addr := startServer(t)
			tc.fn(t, addr)
		})
	}
}
