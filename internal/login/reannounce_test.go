package login

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// countingDirector accepts watch connections and records the SESSION-OPEN and
// SESSION-CLOSE lines each one carries, so a test can see what the login pod
// told the director it reconnected to.
type countingDirector struct {
	ln    net.Listener
	lines chan string
}

func newCountingDirector(t *testing.T) *countingDirector {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &countingDirector{ln: ln, lines: make(chan string, 64)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	return d
}

func (d *countingDirector) serve(conn net.Conn) {
	defer conn.Close()
	_, _ = conn.Write([]byte("DONE\n"))
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\n")
		if strings.HasPrefix(line, "SESSION-") {
			select {
			case d.lines <- line:
			default:
			}
			_, _ = conn.Write([]byte("OK\n"))
		}
	}
}

func (d *countingDirector) await(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-d.lines:
			if strings.HasPrefix(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("the director never received %q", want)
		}
	}
}

// #1393, the mirror half: a session is announced once, to whichever director
// held the watch at the time. When that director dies the pod reconnects --
// often to a different one -- and the session keeps running with nobody
// counting it. The count is then LOW, and a kill waiting for it to reach zero
// confirms at once on a user whose session was never touched.
func TestLiveSessionsAreReannouncedOnReconnect(t *testing.T) {
	d := newCountingDirector(t)
	s := New(Options{DirectorAddr: d.ln.Addr().String(), LocalIP: "127.0.0.1", Protocol: ProtocolIMAP})

	backend, peer := net.Pipe()
	t.Cleanup(func() { backend.Close(); peer.Close() })
	s.sessions = map[string][]*liveSession{
		"u@example.com": {{
			id: "s1", user: "u@example.com", backendConn: backend,
			backendIP: "10.0.0.5", proto: "imap",
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Watch(ctx)

	// The first connection is enough: on it, the pod must announce what it is
	// already running -- that connection IS the reconnect from the director's
	// point of view, since the previous one died with its director.
	d.await(t, "SESSION-OPEN\ts1\tu@example.com\t10.0.0.5")
}

// The close must go through whatever watch connection is current, not the one
// captured when the session started: after a reconnect the captured one is
// dead, and the close would be written into it and lost.
func TestSessionCloseUsesTheCurrentWatch(t *testing.T) {
	d := newCountingDirector(t)
	s := New(Options{DirectorAddr: d.ln.Addr().String(), LocalIP: "127.0.0.1", Protocol: ProtocolIMAP})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Watch(ctx)

	// Wait for a watch to exist, then close a session id through the server
	// rather than through a captured connection.
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.watchMu.RLock()
		wc := s.watch
		s.watchMu.RUnlock()
		if wc != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no watch connection was established")
		}
		time.Sleep(20 * time.Millisecond)
	}

	s.announceSessionClose("s9")
	d.await(t, "SESSION-CLOSE\ts9")
}
