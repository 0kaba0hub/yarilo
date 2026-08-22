package authclient

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// #1402: the jmap and fts user resolvers reach auth on the request path, and
// their classifiers can only tell an outage from a defect if the transport
// failure carries a marker. These rows pin what does and does not carry it --
// the second kind matters as much as the first, since a marker on everything
// classifies nothing.

func TestDialFailureIsUnavailable(t *testing.T) {
	// An address nobody listens on: reserved by binding and closing.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	_, err = Dial(addr, nil)
	if err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("a dial that never reached auth is not marked unavailable: %v", err)
	}
}

// A service that ANSWERS, with something we refuse, is not unavailable. Told
// otherwise, a client would retry a version mismatch forever -- the failure it
// must not retry.
func TestRefusedHandshakeIsNotUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Major version 2: a complete, well-formed answer this client refuses.
		_, _ = conn.Write([]byte("VERSION\tyarilo-auth-master\t2\t0\nDONE\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	_, err = Dial(ln.Addr().String(), nil)
	if err == nil {
		t.Fatal("a major version this client does not speak was accepted")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("a version the service answered with is marked as an outage: %v", err)
	}
}

// A handshake cut mid-line is a transport failure: the service did not answer,
// it went away while answering.
func TestTruncatedHandshakeIsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("VERSION\tyarilo-auth-master\t1\t0\n"))
		conn.Close() // no DONE: the read below fails
	}()

	_, err = Dial(ln.Addr().String(), nil)
	if err == nil {
		t.Fatal("a truncated handshake was accepted")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("a handshake cut mid-stream is not marked unavailable: %v", err)
	}
}

// The deadline/cancellation split, which is the one a marker gets wrong by
// default: an elapsed request budget is the service failing to answer, while a
// cancelled context is our own caller walking away. Reporting the second as an
// outage would blame yarilo-auth for clients that disconnected.
func TestDeadlineIsUnavailableButCancellationIsNot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("VERSION\tyarilo-auth-master\t1\t0\nDONE\n"))
		accepted <- conn // held open, never answering the USER lookup
	}()

	c, err := Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	defer func() { (<-accepted).Close() }()

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := c.Userdb(expired, "u@example.com"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("an elapsed request budget is not marked unavailable: %v", err)
	}

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := c.Userdb(cancelled, "u@example.com"); errors.Is(err, ErrUnavailable) {
		t.Errorf("a caller who walked away is reported as an auth outage: %v", err)
	}
}
