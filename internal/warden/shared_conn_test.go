package warden

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests guard the per-(user, ip) connection accounting against the shared
// long-lived warden connections planned in #878. They are unit tests on purpose:
// the sandbox runs with max_user_ip_connections: 0, which short-circuits
// Limiter.Acquire before it counts anything, so a load test there cannot catch
// a regression in this logic.

// testServer is a live warden listener plus an accept counter, so a test can
// assert how many connections a client actually opened.
type testServer struct {
	*Server
	mu      sync.Mutex
	accepts int
}

func (ts *testServer) acceptCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.accepts
}

// startTestServer runs a real warden listener and returns it with its address.
func startTestServer(t *testing.T, max int, opts ...ServerOption) (*testServer, string) {
	t.Helper()
	ts := &testServer{Server: NewServer(max, opts...)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			ts.mu.Lock()
			ts.accepts++
			ts.mu.Unlock()
			go ts.handleConn(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ts, ln.Addr().String()
}

// testConn is a raw client that speaks the warden wire protocol, so a single
// connection can carry commands for many sessions — the shape the shared-client
// change introduces.
type testConn struct {
	c  net.Conn
	rd *bufio.Reader
}

func dialTestConn(t *testing.T, addr string) *testConn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	rd := bufio.NewReader(c)
	// Drain the VERSION/DONE banner.
	for {
		line, rerr := rd.ReadString('\n')
		if rerr != nil {
			t.Fatalf("handshake: %v", rerr)
		}
		if strings.TrimRight(line, "\r\n") == "DONE" {
			break
		}
	}
	t.Cleanup(func() { _ = c.Close() })
	return &testConn{c: c, rd: rd}
}

func (tc *testConn) cmd(t *testing.T, format string, args ...any) string {
	t.Helper()
	if _, err := fmt.Fprintf(tc.c, format+"\n", args...); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := tc.rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// TestAccountingIsPerUserIPNotPerConnection is the core guarantee: the limiter
// keys on the (user, ip) pair carried in the CONNECT arguments, so routing many
// sessions over ONE connection must count exactly the same as one connection
// per session.
func TestAccountingIsPerUserIPNotPerConnection(t *testing.T) {
	const max = 2
	_, addr := startTestServer(t, max)
	shared := dialTestConn(t, addr)

	tests := []struct {
		name    string
		id      string
		wantOK  bool
		wantErr string
	}{
		{name: "first session of user", id: "s1", wantOK: true},
		{name: "second session of user fills the limit", id: "s2", wantOK: true},
		{name: "third session over the same conn is refused", id: "s3", wantOK: false,
			wantErr: "reason=too-many-connections"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shared.cmd(t, "CONNECT\t%s\tu@d\t10.0.0.1\timap", tc.id)
			if tc.wantOK {
				if !strings.HasPrefix(got, "OK\t") {
					t.Fatalf("CONNECT %s = %q, want OK", tc.id, got)
				}
				return
			}
			if !strings.HasPrefix(got, "FAIL\t") || !strings.Contains(got, tc.wantErr) {
				t.Fatalf("CONNECT %s = %q, want FAIL with %s", tc.id, got, tc.wantErr)
			}
		})
	}

	// The limit must be enforced across connections too — a second connection
	// is not a fresh budget.
	other := dialTestConn(t, addr)
	if got := other.cmd(t, "CONNECT\ts4\tu@d\t10.0.0.1\timap"); !strings.Contains(got, "too-many-connections") {
		t.Fatalf("CONNECT on a second conn = %q, want refusal", got)
	}

	// A different (user, ip) has its own budget.
	if got := other.cmd(t, "CONNECT\ts5\tother@d\t10.0.0.1\timap"); !strings.HasPrefix(got, "OK\t") {
		t.Fatalf("CONNECT for a different user = %q, want OK", got)
	}
}

// TestDisconnectReleasesRegardlessOfConnection covers the release path for
// shared connections: DISCONNECT carries (user, ip) on the wire, so it frees the
// slot even when it arrives on a different connection than the CONNECT did.
func TestDisconnectReleasesRegardlessOfConnection(t *testing.T) {
	s, addr := startTestServer(t, 1)
	first := dialTestConn(t, addr)
	second := dialTestConn(t, addr)

	if got := first.cmd(t, "CONNECT\ts1\tu@d\t10.0.0.1\timap"); !strings.HasPrefix(got, "OK\t") {
		t.Fatalf("CONNECT = %q", got)
	}
	if got := second.cmd(t, "CONNECT\ts2\tu@d\t10.0.0.1\timap"); !strings.Contains(got, "too-many-connections") {
		t.Fatalf("second CONNECT = %q, want refusal", got)
	}
	// Release from the OTHER connection.
	if got := second.cmd(t, "DISCONNECT\ts1\tu@d\t10.0.0.1\timap"); !strings.HasPrefix(got, "OK\t") {
		t.Fatalf("DISCONNECT = %q", got)
	}
	if got := second.cmd(t, "CONNECT\ts3\tu@d\t10.0.0.1\timap"); !strings.HasPrefix(got, "OK\t") {
		t.Fatalf("CONNECT after release = %q, want OK", got)
	}
	if n := len(s.Sessions()); n != 1 {
		t.Fatalf("session count = %d, want 1", n)
	}
}

// TestConnectionDropDoesNotReleaseSlots pins the invariant that makes shared
// connections safe in the first place: the server performs no per-connection
// cleanup, so dropping a connection must NOT free accounting slots. If this
// ever changes, a shared connection dropping would silently release the counts
// of every session riding it and the limit would under-count.
func TestConnectionDropDoesNotReleaseSlots(t *testing.T) {
	s, addr := startTestServer(t, 1)
	doomed := dialTestConn(t, addr)
	if got := doomed.cmd(t, "CONNECT\ts1\tu@d\t10.0.0.1\timap"); !strings.HasPrefix(got, "OK\t") {
		t.Fatalf("CONNECT = %q", got)
	}
	_ = doomed.c.Close()

	// Give the server's handleConn goroutine time to observe the EOF.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(s.Sessions()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(s.Sessions()); n != 1 {
		t.Fatalf("session count after conn drop = %d, want 1 (no per-conn cleanup)", n)
	}

	fresh := dialTestConn(t, addr)
	if got := fresh.cmd(t, "CONNECT\ts2\tu@d\t10.0.0.1\timap"); !strings.Contains(got, "too-many-connections") {
		t.Fatalf("CONNECT after conn drop = %q, want refusal — the slot must still be held", got)
	}
}

// TestHeartbeatOverSharedConnectionKeepsSessionsAlive covers the new failure
// mode shared connections introduce: one connection now carries the heartbeats
// of many sessions, so a single stalled connection could let the sweeper reap
// live sessions and wrongly release their slots.
func TestHeartbeatOverSharedConnectionKeepsSessionsAlive(t *testing.T) {
	const ttl = 200 * time.Millisecond
	s, addr := startTestServer(t, 0, WithSessionTTL(ttl))
	shared := dialTestConn(t, addr)

	ids := []string{"s1", "s2", "s3"}
	for _, id := range ids {
		if got := shared.cmd(t, "CONNECT\t%s\tu@d\t10.0.0.1\timap", id); !strings.HasPrefix(got, "OK\t") {
			t.Fatalf("CONNECT %s = %q", id, got)
		}
	}

	// Heartbeat every session over the single shared connection, twice across
	// the TTL window, then assert none was reaped.
	for range 4 {
		time.Sleep(ttl / 4)
		for _, id := range ids {
			got := shared.cmd(t, "HEARTBEAT\t%s", id)
			if strings.Contains(got, "reason=unknown") {
				t.Fatalf("HEARTBEAT %s = %q — session reaped while heartbeating", id, got)
			}
		}
		s.state.Maintain(time.Now().UTC())
	}

	if n := len(s.Sessions()); n != len(ids) {
		t.Fatalf("session count = %d, want %d", n, len(ids))
	}
}
