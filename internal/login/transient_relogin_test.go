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

// TestTransientReloginCap pins the budget semantics of the client-side re-LOGIN
// cap, including the negative opt-out — an operator disabling it must get the
// pre-#896 close-on-first-transient back, not the default silently applying.
func TestTransientReloginCap(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{"zero selects the default", 0, defaultTransientReloginCap},
		{"explicit budget", 5, 5},
		{"one", 1, 1},
		{"negative opts out", -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{opts: Options{TransientReloginCap: tc.configured}}
			if got := s.transientReloginCap(); got != tc.want {
				t.Fatalf("transientReloginCap() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTransientReloginKeepsConnectionOpen is the #896 acceptance shape: a
// transient failure answers a tagged NO [UNAVAILABLE] but does NOT close the
// connection — the client can LOGIN again on the SAME socket. An unreachable
// yarilo-auth makes every attempt transient (auth_dial), so the connection
// survives the first NO, answers a second, and is closed only at the cap.
func TestTransientReloginKeepsConnectionOpen(t *testing.T) {
	s := &Server{
		opts: Options{
			Protocol:            ProtocolIMAP,
			AuthAddr:            reservedDeadAddr(t), // unreachable → transient auth_dial
			TransientRetries:    -1,                  // fail the dial on the first error
			TransientReloginCap: 2,
		},
		sessions: make(map[string][]*liveSession),
	}
	srv, cli := pipePair(t)
	go s.handleConn(srv)

	crd := bufio.NewReader(cli)
	if _, err := crd.ReadString('\n'); err != nil { // greeting
		t.Fatalf("greeting: %v", err)
	}

	// First LOGIN → tagged NO [UNAVAILABLE], connection stays open.
	if _, err := cli.Write([]byte("a1 LOGIN alice secret\r\n")); err != nil {
		t.Fatalf("write a1: %v", err)
	}
	resp1 := readTagged(t, crd, "a1")
	if !strings.Contains(resp1, "NO") || !strings.Contains(resp1, "UNAVAILABLE") {
		t.Fatalf("first response = %q, want tagged NO [UNAVAILABLE]", resp1)
	}

	// The connection MUST still be usable: a second LOGIN is answered on it.
	if _, err := cli.Write([]byte("a2 LOGIN alice secret\r\n")); err != nil {
		t.Fatalf("write a2 on the same connection: %v", err)
	}
	resp2 := readTagged(t, crd, "a2")
	if !strings.Contains(resp2, "NO") || !strings.Contains(resp2, "UNAVAILABLE") {
		t.Fatalf("second response = %q, want tagged NO [UNAVAILABLE]", resp2)
	}

	// Cap (2) reached: an untagged BYE announces the close (#928), then the
	// connection is closed.
	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	bye, err := crd.ReadString('\n')
	if err != nil {
		t.Fatalf("expected a BYE close notice at the cap: %v", err)
	}
	if !strings.Contains(bye, "BYE") {
		t.Fatalf("cap close notice = %q, want an untagged BYE", bye)
	}
	if _, err := crd.ReadString('\n'); err == nil {
		t.Fatal("connection should be closed after the BYE at the re-login cap")
	}
}

// TestTransientReloginReleasesWardenSlot is the regression test for the leak
// found in review: a transient failure at the BACKEND stage happens after
// warden.Connect, so re-entering the command loop without releasing the warden
// slot would leak a connection-limit slot on every retry. The slot must be back
// to zero once the pass fails.
func TestTransientReloginReleasesWardenSlot(t *testing.T) {
	wardenAddr, wardenSrv := startWardenWithHandle(t)
	authAddr := startOKAuth(t)

	// A backend address that refuses connections, so bring-up fails after warden
	// has already registered the session.
	deadBackend := reservedDeadAddr(t)

	s := &Server{
		opts: Options{
			Protocol:            ProtocolIMAP,
			AuthAddr:            authAddr,
			WardenAddr:          wardenAddr,
			BackendAddr:         deadBackend,
			TransientRetries:    -1, // fail the backend bring-up on the first error
			TransientReloginCap: 1,  // close after the first transient
		},
		sessions: make(map[string][]*liveSession),
	}
	// Close the shared warden pool before the embedded warden server is torn down,
	// or warden's graceful Serve would block on wg.Wait for handlers reading on
	// the still-open pool connections. LIFO ordering runs this before the warden
	// helper's own cleanup. A test artifact only — a real login pod's pool lives
	// for the pod's lifetime.
	t.Cleanup(func() {
		if s.wardenPool != nil {
			s.wardenPool.Close()
		}
	})
	srv, cli := pipePair(t)
	done := make(chan struct{})
	go func() { s.handleConn(srv); close(done) }()

	crd := bufio.NewReader(cli)
	crd.ReadString('\n')                           //nolint:errcheck // greeting
	cli.Write([]byte("a1 LOGIN alice secret\r\n")) //nolint:errcheck
	resp := readTagged(t, crd, "a1")
	if !strings.Contains(resp, "NO") {
		t.Fatalf("response = %q, want a tagged NO after backend bring-up failed", resp)
	}
	cli.Close()
	<-done

	// The warden slot taken during the failed pass must have been released.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if wardenSrv.SessionCount() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("warden session slot leaked: SessionCount = %d, want 0", wardenSrv.SessionCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readTagged reads lines until one starts with tag, returning that line. Untagged
// (* ...) lines are skipped.
func readTagged(t *testing.T, rd *bufio.Reader, tag string) string {
	t.Helper()
	for i := 0; i < 20; i++ {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read tagged %q: %v", tag, err)
		}
		if strings.HasPrefix(line, tag+" ") {
			return strings.TrimRight(line, "\r\n")
		}
	}
	t.Fatalf("no %q-tagged response", tag)
	return ""
}

// reservedDeadAddr binds a port, closes it, and returns the address — almost
// certainly free, so a dial to it is refused.
func reservedDeadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// startWardenWithHandle runs an in-process warden and returns its address plus the
// server, so a test can read SessionCount directly.
func startWardenWithHandle(t *testing.T) (string, *warden.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := warden.NewServer(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, addr, nil); close(done) }()
	waitDial(t, addr)
	t.Cleanup(func() { cancel(); <-done })
	return addr, srv
}

// startOKAuth runs a minimal yarilo-auth wire server that authenticates every
// AUTH request successfully. Enough for tests that need to get PAST auth.
func startOKAuth(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveOKAuth(c)
		}
	}()
	return ln.Addr().String()
}

func serveOKAuth(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	fmt.Fprint(c, "VERSION\t1\t0\nMECH\tPLAIN\tplaintext\nSPID\t1\nDONE\n")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "VERSION":
			continue
		case "AUTH":
			fmt.Fprintf(c, "OK\t%s\tuser=alice\n", fields[1])
		default:
			fmt.Fprintf(c, "FAIL\t%s\n", fields[1])
		}
	}
}

func waitDial(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}
