package login

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

// fakeDirectorPush accepts one watch connection, completes the minimal
// handshake, and pushes the given lines. Returns the listener address.
func fakeDirectorPush(t *testing.T, lines ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("DONE\n"))
		rd := bufio.NewReader(conn)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if line == "DONE\n" {
				break
			}
		}
		for _, l := range lines {
			_, _ = conn.Write([]byte(l + "\n"))
		}
		// Hold the connection open so the read loop stays alive.
		_, _ = rd.ReadString('\n')
	}()
	return ln.Addr().String()
}

// TestWatch_MoveKickReachesTheSession is the #1363 seam: a director that
// authored a move writes the kick to its OWN logins, and that line is the one
// this pod must act on. The whole push is fed through the read loop rather than
// the parser alone, because the defect was in what the loop handed the parser.
//
// The two-field form is the mixed-rollout case: an older originator still
// writes its ring line here, and the trailing backend must not become part of
// the username.
func TestWatch_MoveKickReachesTheSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		push string
	}{
		{"plain kick", "USER-KICKED\tu@example.com"},
		{"move kick with the ring's old-backend field", "USER-KICKED\tu@example.com\t10.0.0.20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := fakeDirectorPush(t, tc.push)
			s := New(Options{DirectorAddr: addr, LocalIP: "127.0.0.1"})

			backend, peer := net.Pipe()
			t.Cleanup(func() { backend.Close(); peer.Close() })
			s.sessions = map[string][]*liveSession{
				"u@example.com": {{id: "s1", user: "u@example.com", backendConn: backend}},
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go s.runWatch(ctx)

			// The kick closes the backend connection; the peer read returns.
			_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := peer.Read(make([]byte, 1)); err == nil {
				t.Fatal("expected the backend connection to be closed by the kick")
			} else if isTimeout(err) {
				t.Fatalf("the kick never reached the session: %v", err)
			}
		})
	}
}

// TestWatch_MalformedKickIsRefused: a form nobody writes on purpose must not be
// trimmed into a plausible username. Nothing is kicked, and the loop survives.
func TestWatch_MalformedKickIsRefused(t *testing.T) {
	for _, push := range []string{
		"USER-KICKED\t",
		"USER-KICKED\tu@example.com\t10.0.0.20\textra",
	} {
		if _, ok := kickedUser(push); ok {
			t.Errorf("kickedUser(%q) accepted a malformed push", push)
		}
	}
	if u, ok := kickedUser("USER-KICKED\tu@example.com\t10.0.0.20"); !ok || u != "u@example.com" {
		t.Errorf("move kick: got %q %v, want the username alone", u, ok)
	}
}

func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	te, ok := err.(timeouter)
	return ok && te.Timeout()
}
