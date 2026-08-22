package authclient

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// #1369: a process starting while auth is rolling has nobody to tell, so
// exiting immediately turns a few seconds of dependency downtime into a
// restart loop. It waits instead -- and the wait is bounded, because a
// dependency that never comes back must not leave a pod hanging silently.
func TestDialWaitingConnectsOnceTheServiceAppears(t *testing.T) {
	// A port nobody is listening on yet: the address is reserved by binding
	// and closing, so the dial fails until the listener is re-opened on it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	go func() {
		time.Sleep(400 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// The client completes a handshake before it calls the dial a
		// success, so a listener that only accepts is still "not up" as far as
		// the wait is concerned -- which is the right definition, and one this
		// test learned by failing.
		_, _ = conn.Write([]byte("VERSION\tyarilo-auth-master\t1\t0\nDONE\n"))
		time.Sleep(2 * time.Second)
		conn.Close()
	}()

	start := time.Now()
	c, err := DialWaiting(context.Background(), addr, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("the dial gave up on a service that came up in 400ms: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("connected in %v, before the listener existed: the test is not exercising the wait", elapsed)
	}
}

// The wait is bounded and says what it waited for. A process that gives up
// after waiting must not report the same dial error as one that never tried.
func TestDialWaitingGivesUpLoudly(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	start := time.Now()
	_, err = DialWaiting(context.Background(), addr, nil, 500*time.Millisecond)
	if err == nil {
		t.Fatal("connected to a service that does not exist")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waited %v for a 500ms bound", elapsed)
	}
	if !strings.Contains(err.Error(), "not reachable after") {
		t.Errorf("the error does not say a wait happened: %v", err)
	}
}

// A zero wait is the historical behaviour, so turning the waiting off needs no
// second code path.
func TestDialWaitingWithoutWaitFailsImmediately(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	start := time.Now()
	if _, err := DialWaiting(context.Background(), addr, nil, 0); err == nil {
		t.Fatal("connected to a service that does not exist")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a zero wait took %v", elapsed)
	}
}
