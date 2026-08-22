package director

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// #1393, second path: a director kept counting sessions whose SESSION-CLOSE
// never arrived. Announcing opens and closes alone leaves one lost event wrong
// forever, because nothing ever says "this is all of it". The login pod's full
// list is that statement.
func TestSessionSyncDropsWhatThePodNoLongerHas(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	for _, id := range []string{"s1", "s2", "s3"} {
		fmt.Fprintf(conn, "SESSION-OPEN\t%s\tu@example.com\t10.0.0.20\n", id)
		if got := readLine(t, sc); got != "OK" {
			t.Fatalf("SESSION-OPEN %s: %q", id, got)
		}
	}

	// The pod says it is running only s2 now: s1 and s3 ended while nobody was
	// listening.
	fmt.Fprint(conn, "SESSION-SYNC-START\nSESSION-SYNC\ts2\nSESSION-SYNC-END\n")
	waitFor(t, 3*time.Second, func() bool { return len(sessionIDs(srv)) == 1 })

	got := sessionIDs(srv)
	if !got["s2"] {
		t.Errorf("the session the pod still has was dropped: %v", got)
	}
	if got["s1"] || got["s3"] {
		t.Errorf("sessions the pod no longer has survived the reconciliation: %v", got)
	}
}

// The list speaks for the pod that sent it and nothing else: another pod's
// sessions and other directors' replicas are none of its business.
func TestSessionSyncIsScopedToItsOwnConnection(t *testing.T) {
	srv, addr := startServer(t)

	connA, scA := dialTest(t, addr)
	readHandshake(t, scA)
	sendHandshake(t, connA)
	fmt.Fprint(connA, "SESSION-OPEN\ta1\tu@example.com\t10.0.0.20\n")
	readLine(t, scA)

	connB, scB := dialTest(t, addr)
	readHandshake(t, scB)
	sendHandshake(t, connB)
	fmt.Fprint(connB, "SESSION-OPEN\tb1\tv@example.com\t10.0.0.21\n")
	readLine(t, scB)

	// A replica of another director, which no login pod owns.
	srv.membership.handleRingLine([]string{
		"SESSION-OPEN", "10.0.0.99@run", "9102", "1", "r1", "w@example.com", "10.0.0.22", "imap",
	}, nil)

	// Pod A says it has nothing.
	fmt.Fprint(connA, "SESSION-SYNC-START\nSESSION-SYNC-END\n")
	waitFor(t, 3*time.Second, func() bool { return !sessionIDs(srv)["a1"] })

	got := sessionIDs(srv)
	if got["a1"] {
		t.Error("the sending pod's stale session survived")
	}
	if !got["b1"] {
		t.Error("another login pod's session was dropped by a list that says nothing about it")
	}
	if !got["r1"] {
		t.Error("another director's replica was dropped by a login pod's list")
	}
}

// Half a list must never be applied: a connection that drops mid-sync would
// otherwise erase every session the pod is still running.
func TestIncompleteSessionSyncChangesNothing(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)
	fmt.Fprint(conn, "SESSION-OPEN\ts1\tu@example.com\t10.0.0.20\n")
	readLine(t, sc)

	// START and one chunk, no END -- then a command that proves the director
	// processed everything up to here.
	fmt.Fprint(conn, "SESSION-SYNC-START\nSESSION-SYNC\ts9\n")
	fmt.Fprint(conn, "SESSION-OPEN\ts2\tu@example.com\t10.0.0.20\n")
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("SESSION-OPEN after a partial sync: %q", got)
	}

	if got := sessionIDs(srv); !got["s1"] {
		t.Errorf("an unfinished list took a live session with it: %v", got)
	}
}

// An unknown command from a login pod is ignored and counted, never fatal to
// the connection: during a rollout a newer pod speaks to an older director for
// one iteration, and dropping the connection would take its sessions with it.
func TestUnknownClientCommandIsCountedNotFatal(t *testing.T) {
	srv, addr := startServer(t)
	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)

	before := testutilCounter(t, "SOMETHING-NEW")
	fmt.Fprint(conn, "SOMETHING-NEW\tx\n")
	fmt.Fprint(conn, "SESSION-OPEN\ts1\tu@example.com\t10.0.0.20\n")
	if got := readLine(t, sc); got != "OK" {
		t.Fatalf("the connection did not survive an unknown command: %q", got)
	}
	if after := testutilCounter(t, "SOMETHING-NEW"); after != before+1 {
		t.Errorf("unknown command counter = %v, want %v", after, before+1)
	}
	_ = srv
}

func testutilCounter(t *testing.T, verb string) float64 {
	t.Helper()
	return testutil.ToFloat64(clientCommandUnknown.WithLabelValues(verb))
}

var _ prometheus.Counter = clientCommandUnknown.WithLabelValues("x")
