package managesieve

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

// tcpPair returns a connected pair over loopback. net.Pipe is not enough here:
// closing one of its ends yields io.ErrClosedPipe, and the defect is specific to
// io.EOF -- the error a peer that hangs up actually produces (#1668).
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	done := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		done <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-done
	if server == nil {
		t.Fatal("accept failed")
	}
	return client, server
}

// A client that hangs up without LOGOUT ends its session.
//
// readAtom turned io.EOF into ("", nil), which serve reads as a blank line and
// loops on -- so every closed connection left a goroutine reading EOF as fast as
// the scheduler allowed. Six of them were live in one sandbox dump, and the
// process burnt 96 minutes of CPU in three hours of wall clock (#1668).
//
// The assertion is that serve returns, not that some error was logged: a spin is
// invisible in the logs and shows up only as a goroutine that never finishes.
func TestAClosedConnectionEndsTheSession(t *testing.T) {
	client, server := tcpPair(t)
	sess := &session{
		conn:     server,
		r:        bufio.NewReader(server),
		w:        bufio.NewWriter(server),
		username: "u1@example.com",
		homeDir:  t.TempDir(),
		store:    newTestStore(),
		maxSize:  65536,
	}
	// A context that outlives the test: the shutdown check at the top of the
	// loop is the one exit that did work, and it would pass this test for the
	// wrong reason.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		sess.serve(ctx)
		close(returned)
	}()

	// Drain the greeting, then hang up mid-session the way a client that is
	// killed does -- no LOGOUT, no CRLF.
	br := bufio.NewReader(client)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("greeting: %v", err)
		}
		if len(line) > 2 && line[:2] == "OK" {
			break
		}
	}
	client.Close() //nolint:errcheck

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the session was still running 5s after the client hung up: " +
			"EOF is being read as a blank line, so the goroutine spins for the life of the process")
	}
}
