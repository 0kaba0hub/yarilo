package sasllogin_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/sasllogin"
)

// fakeAuth is a minimal Dovecot auth server stub.
// It sends the handshake, then echoes OK for AUTH commands and FAIL otherwise.
type fakeAuth struct {
	ln      net.Listener
	results chan authEvent
}

type authEvent struct {
	mech string
	resp string // "OK" or "FAIL"
}

func startFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAuth{ln: ln, results: make(chan authEvent, 16)}
	go fa.run()
	t.Cleanup(func() { ln.Close() })
	return fa
}

func (fa *fakeAuth) addr() string { return fa.ln.Addr().String() }

func (fa *fakeAuth) run() {
	for {
		conn, err := fa.ln.Accept()
		if err != nil {
			return
		}
		go fa.handle(conn)
	}
}

func (fa *fakeAuth) handle(conn net.Conn) {
	defer conn.Close()

	// Send yarilo-auth protocol handshake.
	fmt.Fprintf(conn, "VERSION\tyarilo-auth\t1\t0\n")
	fmt.Fprintf(conn, "MECH\tPLAIN\tplaintext\n")
	fmt.Fprintf(conn, "MECH\tLOGIN\tplaintext\n")
	fmt.Fprintf(conn, "SPID\t1234\n")
	fmt.Fprintf(conn, "CUID\t1\n")
	fmt.Fprintf(conn, "COOKIE\tdeadbeef\n")
	fmt.Fprintf(conn, "DONE\n")

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.Split(line, "\t")
		if len(parts) < 3 || parts[0] != "AUTH" {
			continue
		}
		id := parts[1]
		mech := parts[2]

		// Extract user from resp= field if PLAIN.
		result := "OK"
		for _, p := range parts[3:] {
			if p == "fail" {
				result = "FAIL"
			}
		}

		fa.results <- authEvent{mech: mech, resp: result}

		if result == "OK" {
			fmt.Fprintf(conn, "OK\t%s\tuser=testuser\n", id)
		} else {
			fmt.Fprintf(conn, "FAIL\t%s\treason=auth failed\n", id)
		}
	}
}

// startProxy starts a sasllogin.Server proxying to the given authAddr and
// returns the proxy's listener address.
func startProxy(t *testing.T, authAddr string, opts sasllogin.Options) string {
	t.Helper()
	opts.AuthAddr = authAddr
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := sasllogin.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }() //nolint:errcheck
	return ln.Addr().String()
}

// dialProxy connects to the proxy and reads the server handshake lines
// up to and including DONE.
func dialProxy(t *testing.T, addr string) (net.Conn, []string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(conn)
	var handshake []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		handshake = append(handshake, line)
		if line == "DONE" {
			break
		}
	}
	return conn, handshake
}

func TestProxy_Handshake(t *testing.T) {
	fa := startFakeAuth(t)
	addr := startProxy(t, fa.addr(), sasllogin.Options{})

	_, handshake := dialProxy(t, addr)

	wantLines := []string{"VERSION\tyarilo-auth\t1\t0", "MECH\tPLAIN\tplaintext", "MECH\tLOGIN\tplaintext", "DONE"}
	for _, want := range wantLines {
		found := false
		for _, got := range handshake {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("handshake missing %q; got %v", want, handshake)
		}
	}
}

func TestProxy_AuthOK(t *testing.T) {
	fa := startFakeAuth(t)
	addr := startProxy(t, fa.addr(), sasllogin.Options{})

	conn, _ := dialProxy(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "VERSION\t1\t0\n")
	fmt.Fprintf(conn, "CPID\t9999\n")
	fmt.Fprintf(conn, "AUTH\t1\tPLAIN\tservice=smtp\trip=1.2.3.4\tlip=10.0.0.1\tresp=dGVzdA==\n")

	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "OK\t1\t") {
		t.Errorf("expected OK response, got %q", line)
	}
	if !strings.Contains(line, "user=testuser") {
		t.Errorf("expected user=testuser in response, got %q", line)
	}

	select {
	case ev := <-fa.results:
		if ev.mech != "PLAIN" {
			t.Errorf("auth event mech = %q, want PLAIN", ev.mech)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for auth event")
	}
}

func TestProxy_AuthFail(t *testing.T) {
	fa := startFakeAuth(t)
	addr := startProxy(t, fa.addr(), sasllogin.Options{})

	conn, _ := dialProxy(t, addr)
	defer conn.Close()

	fmt.Fprintf(conn, "VERSION\t1\t0\n")
	fmt.Fprintf(conn, "CPID\t9999\n")
	fmt.Fprintf(conn, "AUTH\t2\tPLAIN\tservice=smtp\trip=1.2.3.4\tfail\n")

	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "FAIL\t2\t") {
		t.Errorf("expected FAIL response, got %q", line)
	}
}

func TestProxy_TrustedNets_Reject(t *testing.T) {
	fa := startFakeAuth(t)
	_, reject, _ := net.ParseCIDR("192.168.0.0/24")
	addr := startProxy(t, fa.addr(), sasllogin.Options{
		TrustedNets: []*net.IPNet{reject},
	})

	// 127.0.0.1 is not in 192.168.0.0/24 — connection should be closed immediately.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second)) //nolint:errcheck

	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if n != 0 {
		t.Errorf("expected no data from rejected connection, got %d bytes: %q", n, buf[:n])
	}
}

func TestProxy_MultipleClients(t *testing.T) {
	fa := startFakeAuth(t)
	addr := startProxy(t, fa.addr(), sasllogin.Options{})

	const N = 5
	for i := range N {
		i := i
		t.Run(fmt.Sprintf("client%d", i), func(t *testing.T) {
			t.Parallel()
			conn, _ := dialProxy(t, addr)
			defer conn.Close()

			fmt.Fprintf(conn, "VERSION\t1\t0\n")
			fmt.Fprintf(conn, "CPID\t%d\n", 9000+i)
			fmt.Fprintf(conn, "AUTH\t%d\tPLAIN\tservice=smtp\trip=1.2.3.%d\tresp=dGVzdA==\n", i+1, i)

			rd := bufio.NewReader(conn)
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("client %d read response: %v", i, err)
			}
			if !strings.HasPrefix(strings.TrimRight(line, "\r\n"), "OK\t") {
				t.Errorf("client %d: expected OK, got %q", i, line)
			}
		})
	}
}
