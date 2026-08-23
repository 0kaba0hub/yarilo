package protocol

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The master listener carried no metrics at all, so "how often do the backends
// resolve a user, and do they hold a connection while doing it" had no answer
// (#1402). The client-protocol counters do not answer it: they belong to the
// login proxies, and a flat curve there says nothing about a resolver on this
// listener.
//
// Both counters are read together on purpose. Requests alone cannot tell a
// caller that dials per request from one that holds a connection, which is the
// question that motivated the instrument.
func TestMasterListenerCountsConnectionsAndRequests(t *testing.T) {
	// The readiness probe IS the first connection here, deliberately. A
	// throwaway dial to check the listener is up is itself a connection, and
	// counting it in a test about connection counts made this test race its
	// own setup.
	addr := serveMaster(t, &nonIteratorUserdb{users: map[string]*UserInfo{
		"u@example.com": {Username: "u@example.com", Home: "/srv/u"},
	}})

	conns0 := testutil.ToFloat64(masterConnectionsTotal)
	users0 := testutil.ToFloat64(masterRequestsTotal.WithLabelValues("USER"))

	// Two lookups on ONE connection: the shape a pooled caller produces.
	conn, rd := dialMaster(t, addr)
	drainHandshake(t, rd)
	for i := 1; i <= 2; i++ {
		fmt.Fprintf(conn, "USER\t%d\tu@example.com\n", i)
		if _, err := rd.ReadString('\n'); err != nil {
			t.Fatalf("read reply %d: %v", i, err)
		}
	}

	if got := testutil.ToFloat64(masterRequestsTotal.WithLabelValues("USER")) - users0; got != 2 {
		t.Errorf("USER requests counted = %v, want 2", got)
	}
	if got := testutil.ToFloat64(masterConnectionsTotal) - conns0; got != 1 {
		t.Errorf("connections counted = %v, want 1: two requests on one connection must not read as two dials", got)
	}

	// And a second dial moves the connection counter, so the two curves can
	// diverge -- which is the whole point of counting them separately.
	conn2, rd2 := dialMaster(t, addr)
	drainHandshake(t, rd2)
	fmt.Fprintf(conn2, "USER\t3\tu@example.com\n")
	if _, err := rd2.ReadString('\n'); err != nil {
		t.Fatalf("read reply on the second connection: %v", err)
	}
	if got := testutil.ToFloat64(masterConnectionsTotal) - conns0; got != 2 {
		t.Errorf("connections counted = %v after a second dial, want 2", got)
	}
}

// An unknown verb is counted under a constant label, never under what the
// client sent: a label taken from the wire lets any caller grow the metric's
// cardinality without limit.
func TestUnknownMasterVerbDoesNotBecomeALabel(t *testing.T) {
	addr := serveMaster(t, &nonIteratorUserdb{users: map[string]*UserInfo{}})
	unknown0 := testutil.ToFloat64(masterRequestsTotal.WithLabelValues("unknown"))

	conn, rd := dialMaster(t, addr)
	drainHandshake(t, rd)
	fmt.Fprintf(conn, "WHOAMI\t1\tsomething\n")
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatalf("read reply: %v", err)
	}

	if got := testutil.ToFloat64(masterRequestsTotal.WithLabelValues("unknown")) - unknown0; got != 1 {
		t.Errorf("unknown verb counted = %v, want 1", got)
	}
	if got := testutil.ToFloat64(masterRequestsTotal.WithLabelValues("WHOAMI")); got != 0 {
		t.Errorf("the wire verb became a label: WHOAMI = %v", got)
	}
}

// serveMaster starts a MasterServer and returns its address once it answers,
// without leaving a probe connection behind: readiness is established by the
// first real dial, which the caller then keeps.
func serveMaster(t *testing.T, userdb Userdb) string {
	t.Helper()
	srv := NewMasterServer(userdb)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	t.Cleanup(cancel)
	return addr
}

// dialMaster dials, retrying until the listener is up, and hands back the
// connection it opened.
func dialMaster(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			t.Cleanup(func() { conn.Close() })
			return conn, bufio.NewReader(conn)
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func drainHandshake(t *testing.T, rd *bufio.Reader) {
	t.Helper()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if line == "DONE\n" {
			return
		}
	}
}
