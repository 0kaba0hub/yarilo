package login

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// startSilentBackend accepts connections and then sends NOTHING — modelling a
// backend that is up at the TCP layer but never greets (storage hang, a wedged
// token Verify). The preamble write buffers; readBackendGreeting blocks until
// the bring-up deadline fires.
func startSilentBackend(t *testing.T) string {
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
			// Hold the connection open, mute. Never read, never greet.
			_ = c
		}
	}()
	return ln.Addr().String()
}

// TestBackendBringupDeadlineDoesNotHang is the #927 regression: a backend that
// accepts but never greets must not pin the login handler in readBackendGreeting
// forever. The bring-up times out, becomes a transient failure, and the client
// gets a keep-open NO [UNAVAILABLE] on the same connection — the warden slot
// released — instead of hanging until the client's own deadline.
func TestBackendBringupDeadlineDoesNotHang(t *testing.T) {
	prev := backendBringupTimeout
	backendBringupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { backendBringupTimeout = prev })

	wardenAddr, wardenSrv := startWardenWithHandle(t)
	authAddr := startOKAuth(t)
	silent := startSilentBackend(t)

	s := &Server{
		opts: Options{
			Protocol:            ProtocolIMAP,
			AuthAddr:            authAddr,
			WardenAddr:          wardenAddr,
			BackendAddr:         silent,
			TransientRetries:    -1, // fail the bring-up on the first timeout
			TransientReloginCap: 2,  // keep open for one re-LOGIN, then close
		},
		sessions: make(map[string][]*liveSession),
	}
	t.Cleanup(func() {
		if s.wardenPool != nil {
			s.wardenPool.Close()
		}
	})

	srv, cli := pipePair(t)
	go s.handleConn(srv)
	crd := bufio.NewReader(cli)
	crd.ReadString('\n') //nolint:errcheck // greeting

	// First LOGIN: the mute backend must NOT hang the handler; it must answer a
	// keep-open transient within roughly the bring-up timeout.
	cli.Write([]byte("a1 LOGIN alice secret\r\n")) //nolint:errcheck
	start := time.Now()
	resp1 := readTagged(t, crd, "a1")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("a1 took %v — the bring-up deadline did not bound readBackendGreeting", d)
	}
	if !strings.Contains(resp1, "NO") || !strings.Contains(resp1, "UNAVAILABLE") {
		t.Fatalf("a1 = %q, want tagged NO [UNAVAILABLE]", resp1)
	}

	// The connection is still usable: a second LOGIN is answered on it (#896).
	cli.Write([]byte("a2 LOGIN alice secret\r\n")) //nolint:errcheck
	resp2 := readTagged(t, crd, "a2")
	if !strings.Contains(resp2, "NO") {
		t.Fatalf("a2 = %q, want the connection still open with a tagged NO", resp2)
	}

	// The warden slot taken on each timed-out bring-up must have been released.
	deadline := time.Now().Add(2 * time.Second)
	for wardenSrv.SessionCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("warden slot leaked after a bring-up timeout: SessionCount = %d", wardenSrv.SessionCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
