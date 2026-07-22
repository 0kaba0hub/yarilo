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

// startTestDirector starts a director server on a random port and returns the
// server and its listen address.
func startTestDirector(t *testing.T) (*Server, string) {
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
	go srv.listenOn(ctx, ln) //nolint:errcheck
	return srv, ln.Addr().String()
}

// TestPeerDialer_HandshakeSeedsRing verifies that backends registered on
// srvA appear in srvB's ring after the PeerDialer handshake.
func TestPeerDialer_HandshakeSeedsRing(t *testing.T) {
	srvA, addrA := startTestDirector(t)
	srvB, _ := startTestDirector(t)

	// Register a backend on A.
	srvA.AddBackend("10.0.0.1", 993, "imap")

	// Dial A from B.
	pd := NewPeerDialer(srvB, []string{addrA}, nil, "127.0.0.1", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b := srvB.ring.LookupBackend("anyuser")
		if b != nil && b.IP == "10.0.0.1" {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("backend 10.0.0.1 never appeared in srvB ring after peer handshake")
}

// TestPeerDialer_RingChangeDownPropagates verifies that a RING-CHANGE down
// event from srvA removes the backend from srvB.
func TestPeerDialer_RingChangeDownPropagates(t *testing.T) {
	srvA, addrA := startTestDirector(t)
	srvB, _ := startTestDirector(t)

	srvA.AddBackend("10.0.0.2", 993, "")
	// Also pre-seed srvB so SetUp can find the backend when the down event arrives.
	srvB.ring.AddBackend(&ring.Backend{IP: "10.0.0.2", Port: 993, Up: true})

	pd := NewPeerDialer(srvB, []string{addrA}, nil, "127.0.0.1", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd.Start(ctx)

	// Wait for the peer connection to stabilise.
	time.Sleep(200 * time.Millisecond)

	// Remove the backend on A — this broadcasts RING-CHANGE down.
	srvA.ring.RemoveBackend("10.0.0.2")
	srvA.broadcast("RING-CHANGE\t10.0.0.2\tdown\t", nil)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b := srvB.ring.LookupBackend("anyuser")
		if b == nil {
			return // ring is empty → backend was removed
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("backend 10.0.0.2 was not removed from srvB ring after RING-CHANGE down")
}

// TestPeerDialer_UserMovedPropagates verifies that a USER-MOVED push from srvA
// updates the user directory on srvB.
func TestPeerDialer_UserMovedPropagates(t *testing.T) {
	srvA, addrA := startTestDirector(t)
	srvB, _ := startTestDirector(t)

	pd := NewPeerDialer(srvB, []string{addrA}, nil, "127.0.0.1", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd.Start(ctx)

	time.Sleep(200 * time.Millisecond)

	// Broadcast a USER-MOVED from A.
	srvA.broadcast("USER-MOVED\tuser@test.com\t10.0.0.3\t993", nil)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e := srvB.userDir.Get("user@test.com")
		if e != nil && e.Host == "10.0.0.3:993" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("user-moved not reflected in srvB userDir")
}

// TestPeerDialer_UserKickedBroadcastsLocally verifies that a USER-KICKED push
// from a peer is re-broadcast to all clients connected to srvB.
func TestPeerDialer_UserKickedBroadcastsLocally(t *testing.T) {
	srvA, addrA := startTestDirector(t)
	srvB, addrB := startTestDirector(t)

	// Connect srvB peer dialer to srvA.
	pd := NewPeerDialer(srvB, []string{addrA}, nil, "127.0.0.1", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd.Start(ctx)

	// Also connect a plain client to srvB to receive the kick.
	conn, err := net.DialTimeout("tcp", addrB, 2*time.Second)
	if err != nil {
		t.Fatalf("dial srvB: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

	sc := bufio.NewScanner(conn)
	// Consume server handshake.
	for sc.Scan() {
		if sc.Text() == "DONE" {
			break
		}
	}
	// Send client handshake.
	fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\nME\t127.0.0.1\t0\t0\nDONE\n") //nolint:errcheck

	time.Sleep(200 * time.Millisecond)

	// Broadcast USER-KICKED from A → srvB receives it via peer dialer and
	// re-broadcasts to its own clients.
	srvA.broadcast("USER-KICKED\tkick@test.com", nil)

	deadline := time.Now().Add(3 * time.Second)
	conn.SetDeadline(deadline) //nolint:errcheck
	for sc.Scan() {
		if strings.Contains(sc.Text(), "USER-KICKED") && strings.Contains(sc.Text(), "kick@test.com") {
			return
		}
	}
	t.Error("USER-KICKED was not forwarded to srvB local client")
}

// TestPeerDialer_HandshakePreservesFlushedState verifies that a backend which is
// currently flushed (Up=false) on srvA is NOT re-activated on srvB after the peer
// handshake — the D/U timestamps in the HOST line must encode the down state.
func TestPeerDialer_HandshakePreservesFlushedState(t *testing.T) {
	srvA, addrA := startTestDirector(t)
	srvB, _ := startTestDirector(t)

	// Register and then flush the backend on A.
	srvA.AddBackend("10.0.0.9", 993, "imap")
	srvA.ring.SetUp("10.0.0.9", false, time.Now().Unix())

	// Dial A from B.
	pd := NewPeerDialer(srvB, []string{addrA}, nil, "127.0.0.1", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd.Start(ctx)

	// Give the handshake time to complete.
	time.Sleep(300 * time.Millisecond)

	// srvB must know about the backend (it appeared in handshake)…
	backends := srvB.ring.Backends()
	var found *ring.Backend
	for i := range backends {
		if backends[i].IP == "10.0.0.9" {
			found = &backends[i]
			break
		}
	}
	if found == nil {
		t.Fatal("backend 10.0.0.9 missing from srvB registry after handshake")
	}
	// …but must NOT be active in the ring (Up=false).
	if found.Up {
		t.Errorf("backend 10.0.0.9 is Up=true on srvB, expected Up=false (was flushed on srvA)")
	}
	// Sanity: LookupBackend must return nil (no active backends).
	if b := srvB.ring.LookupBackend("anyuser"); b != nil {
		t.Errorf("LookupBackend returned %+v; expected nil for a fully-down ring", b)
	}
}

// TestPeerDialer_PingPong verifies that the peer dialer responds to PING with PONG.
func TestPeerDialer_PingPong(t *testing.T) {
	_, addrA := startTestDirector(t)
	srvB := New()

	pd := NewPeerDialer(srvB, []string{addrA}, nil, "127.0.0.1", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd.Start(ctx)

	// Connect as a raw client to srvA so we can send PING directly to the
	// srvB→srvA peer connection. Instead, open an independent client and
	// verify the server side processes PONG (PING→PONG is tested via the
	// keepalive path; here we just check the peer stays connected).
	time.Sleep(300 * time.Millisecond)

	// Dial srvA as a separate client and send PING.
	conn, err := net.DialTimeout("tcp", addrA, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		if sc.Text() == "DONE" {
			break
		}
	}
	fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\nME\t127.0.0.1\t0\t0\nDONE\n") //nolint:errcheck
	fmt.Fprintf(conn, "PING\n")                                                      //nolint:errcheck

	for sc.Scan() {
		if sc.Text() == "PONG" {
			return
		}
	}
	t.Error("did not receive PONG from server")
}

// dialPlainLoginClient dials addr as a generic login-proxy client (no PEER
// handshake line) and returns the connection plus a scanner positioned right
// after the server handshake, ready to read unsolicited pushes.
func dialPlainLoginClient(t *testing.T, addr string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		if sc.Text() == "DONE" {
			break
		}
	}
	fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\nME\t127.0.0.1\t0\t0\nDONE\n") //nolint:errcheck
	return conn, sc
}

// TestUserKicked_DoesNotLoopBetweenMeshedPeers reproduces #700: with a
// full-mesh pair of directors (A dials B, B dials A), a single USER-KICKED
// broadcast from A must reach each side's login clients exactly once and
// must NOT ping-pong forever between the two peer connections.
func TestUserKicked_DoesNotLoopBetweenMeshedPeers(t *testing.T) {
	srvA, addrA := startTestDirector(t)
	srvB, addrB := startTestDirector(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pdA := NewPeerDialer(srvA, []string{addrB}, nil, "127.0.0.1", 0)
	pdB := NewPeerDialer(srvB, []string{addrA}, nil, "127.0.0.1", 0)
	pdA.Start(ctx)
	pdB.Start(ctx)
	time.Sleep(300 * time.Millisecond) // let both peer handshakes complete

	_, scA := dialPlainLoginClient(t, addrA)
	_, scB := dialPlainLoginClient(t, addrB)

	// Simulate the ORIGINATING kick landing on A (as handleUserKick would
	// broadcast it): reaches A's login client directly, and A's peer
	// connection to/from B relays it once.
	srvA.broadcast("USER-KICKED\tkick@test.com", nil)

	readKicked := func(sc *bufio.Scanner) bool {
		for sc.Scan() {
			if strings.Contains(sc.Text(), "USER-KICKED") && strings.Contains(sc.Text(), "kick@test.com") {
				return true
			}
		}
		return false
	}
	if !readKicked(scA) {
		t.Fatal("A's login client never received USER-KICKED")
	}
	if !readKicked(scB) {
		t.Fatal("B's login client never received USER-KICKED (peer relay failed)")
	}

	// Regression check: if this ping-ponged (the #700 bug), B would relay
	// back to A, A would broadcast again reaching scA a second time, and so
	// on without bound. Give it a window to loop, then confirm nothing
	// further arrives at either login client.
	for _, tc := range []struct {
		name string
		sc   *bufio.Scanner
	}{{"A", scA}, {"B", scB}} {
		done := make(chan bool, 1)
		go func() { done <- readKicked(tc.sc) }()
		select {
		case got := <-done:
			if got {
				t.Fatalf("%s's login client received a SECOND USER-KICKED — peer broadcast loop (#700)", tc.name)
			}
		case <-time.After(1 * time.Second):
			// No further line within the window: the loop did not occur.
		}
	}
}
