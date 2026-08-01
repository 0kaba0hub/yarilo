package anvil_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/internal/anvil"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func startServer(t *testing.T, max int) (addr string, cancel context.CancelFunc) {
	t.Helper()
	srv := anvil.NewServer(max)
	ctx, cancel := context.WithCancel(context.Background())
	addr = freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)
	return addr, cancel
}

func dial(t *testing.T, addr string) (*bufio.Scanner, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	sc := bufio.NewScanner(conn)

	// drain handshake until DONE
	for sc.Scan() {
		if sc.Text() == "DONE" {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return sc, conn
}

func send(t *testing.T, conn net.Conn, sc *bufio.Scanner, cmd string) string {
	t.Helper()
	fmt.Fprintln(conn, cmd)
	if !sc.Scan() {
		t.Fatalf("no response to %q: %v", cmd, sc.Err())
	}
	return sc.Text()
}

func TestHandshake(t *testing.T) {
	srv := anvil.NewServer(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))

	sc := bufio.NewScanner(conn)
	var lines []string
	for sc.Scan() {
		line := sc.Text()
		lines = append(lines, line)
		if line == "DONE" {
			break
		}
	}
	hs := strings.Join(lines, "\n")
	if !strings.Contains(hs, "VERSION\tyarilo-anvil") {
		t.Errorf("missing VERSION in handshake: %q", hs)
	}
	if !strings.Contains(hs, "DONE") {
		t.Errorf("missing DONE in handshake: %q", hs)
	}
}

func TestConnect_OK(t *testing.T) {
	addr, cancel := startServer(t, 5)
	defer cancel()

	sc, conn := dial(t, addr)
	defer conn.Close()

	resp := send(t, conn, sc, "CONNECT\t1\talice@example.com\t1.2.3.4\timap")
	if !strings.HasPrefix(resp, "OK\t1") {
		t.Errorf("expected OK, got: %q", resp)
	}
}

func TestConnect_LimitExceeded(t *testing.T) {
	addr, cancel := startServer(t, 2)
	defer cancel()

	sc, conn := dial(t, addr)
	defer conn.Close()

	send(t, conn, sc, "CONNECT\t1\talice@example.com\t1.2.3.4\timap")
	send(t, conn, sc, "CONNECT\t2\talice@example.com\t1.2.3.4\timap")
	resp := send(t, conn, sc, "CONNECT\t3\talice@example.com\t1.2.3.4\timap")
	if !strings.HasPrefix(resp, "FAIL\t3") {
		t.Errorf("expected FAIL on 3rd connection, got: %q", resp)
	}
	if !strings.Contains(resp, "too-many-connections") {
		t.Errorf("expected too-many-connections reason, got: %q", resp)
	}
}

func TestDisconnect_ReleasesSlot(t *testing.T) {
	addr, cancel := startServer(t, 1)
	defer cancel()

	sc, conn := dial(t, addr)
	defer conn.Close()

	send(t, conn, sc, "CONNECT\t1\talice@example.com\t1.2.3.4\timap")

	// second connect must fail
	resp := send(t, conn, sc, "CONNECT\t2\talice@example.com\t1.2.3.4\timap")
	if !strings.HasPrefix(resp, "FAIL\t2") {
		t.Errorf("expected FAIL, got: %q", resp)
	}

	// disconnect the connected session (id 1) frees the slot. A login pod always
	// sends DISCONNECT with the same id it CONNECTed, so release is by session id
	// — consistent with the Redis backend's idempotent, id-keyed decrement.
	send(t, conn, sc, "DISCONNECT\t1\talice@example.com\t1.2.3.4\timap")

	// now connect must succeed
	resp = send(t, conn, sc, "CONNECT\t4\talice@example.com\t1.2.3.4\timap")
	if !strings.HasPrefix(resp, "OK\t4") {
		t.Errorf("expected OK after disconnect, got: %q", resp)
	}
}

func TestConnect_DifferentIPs_IndependentLimits(t *testing.T) {
	addr, cancel := startServer(t, 1)
	defer cancel()

	sc, conn := dial(t, addr)
	defer conn.Close()

	send(t, conn, sc, "CONNECT\t1\talice@example.com\t1.2.3.4\timap")

	// same user, different IP — should be allowed
	resp := send(t, conn, sc, "CONNECT\t2\talice@example.com\t5.6.7.8\timap")
	if !strings.HasPrefix(resp, "OK\t2") {
		t.Errorf("different IP should be allowed, got: %q", resp)
	}
}

func TestConnect_Unlimited(t *testing.T) {
	addr, cancel := startServer(t, 0) // 0 = unlimited
	defer cancel()

	sc, conn := dial(t, addr)
	defer conn.Close()

	for i := 1; i <= 100; i++ {
		resp := send(t, conn, sc, fmt.Sprintf("CONNECT\t%d\talice@example.com\t1.2.3.4\timap", i))
		if !strings.HasPrefix(resp, "OK\t") {
			t.Fatalf("unlimited: expected OK on connection %d, got: %q", i, resp)
		}
	}
}

func TestGracefulShutdown(t *testing.T) {
	srv := anvil.NewServer(10)
	ctx, cancel := context.WithCancel(context.Background())

	addr := freeAddr(t)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr, nil) }()
	time.Sleep(20 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("ListenAndServe did not return after cancel")
	}
}
