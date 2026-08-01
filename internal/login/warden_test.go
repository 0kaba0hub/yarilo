package login

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/warden"
)

// startStubAuth starts a minimal yarilo-auth server that always returns OK.
func startStubAuth(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleStubAuth(conn)
		}
	}()
	return ln.Addr().String()
}

func handleStubAuth(conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReader(conn)
	fmt.Fprintf(conn, "VERSION\t1\t0\n")
	fmt.Fprintf(conn, "MECH\tPLAIN\n")
	fmt.Fprintf(conn, "DONE\n")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "VERSION":
			// already sent ours; ignore client's VERSION
		case "AUTH":
			id := fields[1]
			fmt.Fprintf(conn, "OK\t%s\tuser=alice\ttoken=stubtoken1234567890123456789012345678901234567890123456789012\n", id)
		}
	}
}

// startWarden starts a real yarilo-warden server and returns its address.
func startWarden(t *testing.T, max int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := warden.NewServer(max)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)
	return addr
}

// stubDirector starts a minimal director that always returns backendAddr for any LOOKUP.
func stubDirector(t *testing.T, backendAddr string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleStubDirector(conn, backendAddr)
		}
	}()
	return ln.Addr().String()
}

func handleStubDirector(conn net.Conn, backendAddr string) {
	defer conn.Close()
	rd := bufio.NewReader(conn)

	// Send director handshake.
	fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\n")
	fmt.Fprintf(conn, "DONE\n")

	// Read client handshake (VERSION + ME + DONE).
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimRight(line, "\n") == "DONE" {
			break
		}
	}

	// Process commands.
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\n")
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "LOOKUP":
			// LOOKUP\t{id}\t{user}\t{tag}
			// Response: HOST\t{id}\t{ip}\t{port}
			id := fields[1]
			host, port, _ := net.SplitHostPort(backendAddr)
			fmt.Fprintf(conn, "HOST\t%s\t%s\t%s\n", id, host, port)
		}
	}
}

// stubIMAPBackend starts a minimal IMAP backend that accepts any LOGIN/AUTHENTICATE.
func stubIMAPBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleStubIMAPBackend(conn)
		}
	}()
	return ln.Addr().String()
}

func handleStubIMAPBackend(conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReader(conn)
	fmt.Fprintf(conn, "* OK IMAP4rev1 backend ready\r\n")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag := fields[0]
		cmd := strings.ToUpper(fields[1])
		switch cmd {
		case "XCONN":
			// XCONN XCLIENT ADDR=... SESSION=... TOKEN=... USER=...
			fmt.Fprintf(conn, "* OK xconn accepted\r\n")
		case "LOGOUT":
			fmt.Fprintf(conn, "* BYE\r\n%s OK bye\r\n", tag)
			return
		default:
			fmt.Fprintf(conn, "%s OK\r\n", tag)
		}
	}
}

func buildWardenLoginServer(t *testing.T, wardenAddr string, maxConns int) (loginAddr string) {
	t.Helper()
	backendAddr := stubIMAPBackend(t)
	dirAddr := stubDirector(t, backendAddr)
	authAddr := startStubAuth(t)

	srv := New(Options{
		Protocol:       ProtocolIMAP,
		DirectorAddr:   dirAddr,
		WardenAddr:     wardenAddr,
		WardenFailOpen: false,
		AuthAddr:       authAddr,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck
	return ln.Addr().String()
}

func TestLogin_Warden_AllowsSession(t *testing.T) {
	wardenAddr := startWarden(t, 5)
	loginAddr := buildWardenLoginServer(t, wardenAddr, 5)

	conn, err := net.DialTimeout("tcp", loginAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	rd := bufio.NewReader(conn)

	greeting, _ := rd.ReadString('\n')
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("unexpected greeting: %q", greeting)
	}

	fmt.Fprintf(conn, "A1 LOGIN alice secret\r\n")
	resp, _ := rd.ReadString('\n')
	if !strings.HasPrefix(resp, "A1 OK") {
		t.Fatalf("expected A1 OK, got: %q", resp)
	}
}

func TestLogin_Warden_RejectsWhenLimitReached(t *testing.T) {
	wardenAddr := startWarden(t, 1)

	// Pre-fill the limit for alice@127.0.0.1 by dialling warden directly.
	ac, err := warden.Dial(wardenAddr, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	if err := ac.Connect("pre", "alice", "127.0.0.1", "imap"); err != nil {
		t.Fatalf("pre-fill: %v", err)
	}

	loginAddr := buildWardenLoginServer(t, wardenAddr, 1)

	conn, err := net.DialTimeout("tcp", loginAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	rd := bufio.NewReader(conn)

	greeting, _ := rd.ReadString('\n')
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("unexpected greeting: %q", greeting)
	}

	fmt.Fprintf(conn, "A1 LOGIN alice secret\r\n")
	resp, _ := rd.ReadString('\n')
	// Expect NO [LIMIT] too many connections, THEN an untagged BYE announcing
	// the close (#928 consistency) rather than a bare tagged NO then a silent
	// drop.
	if !strings.HasPrefix(resp, "A1 NO") {
		t.Fatalf("expected A1 NO (too many connections), got: %q", resp)
	}
	bye, _ := rd.ReadString('\n')
	if !strings.HasPrefix(bye, "* BYE") {
		t.Fatalf("expected a * BYE close notice after the over-limit NO, got: %q", bye)
	}
}

func TestLogin_Warden_FailOpen_WhenUnreachable(t *testing.T) {
	backendAddr := stubIMAPBackend(t)
	dirAddr := stubDirector(t, backendAddr)
	authAddr := startStubAuth(t)

	srv := New(Options{
		Protocol:       ProtocolIMAP,
		DirectorAddr:   dirAddr,
		WardenAddr:     "127.0.0.1:1", // unreachable
		WardenFailOpen: true,
		AuthAddr:       authAddr,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	rd := bufio.NewReader(conn)

	greeting, _ := rd.ReadString('\n')
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("unexpected greeting: %q", greeting)
	}

	fmt.Fprintf(conn, "A1 LOGIN alice secret\r\n")
	resp, _ := rd.ReadString('\n')
	// Fail-open: session should proceed despite warden being unreachable.
	if !strings.HasPrefix(resp, "A1 OK") {
		t.Fatalf("expected A1 OK (fail-open), got: %q", resp)
	}
}

func TestLogin_Warden_FailClosed_WhenUnreachable(t *testing.T) {
	backendAddr := stubIMAPBackend(t)
	dirAddr := stubDirector(t, backendAddr)
	authAddr := startStubAuth(t)

	srv := New(Options{
		Protocol:       ProtocolIMAP,
		DirectorAddr:   dirAddr,
		WardenAddr:     "127.0.0.1:1", // unreachable
		WardenFailOpen: false,
		AuthAddr:       authAddr,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	rd := bufio.NewReader(conn)

	greeting, _ := rd.ReadString('\n')
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("unexpected greeting: %q", greeting)
	}

	fmt.Fprintf(conn, "A1 LOGIN alice secret\r\n")
	resp, _ := rd.ReadString('\n')
	if !strings.HasPrefix(resp, "A1 NO") {
		t.Fatalf("expected A1 NO (fail-closed), got: %q", resp)
	}
}
