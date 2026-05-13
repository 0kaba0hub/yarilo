package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func newMaxLineLenTestConn(t *testing.T, limit int) (client net.Conn, server *maxLineLenConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		conn *maxLineLenConn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ln.Close() //nolint:errcheck
		if err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{&maxLineLenConn{Conn: c, br: bufio.NewReader(c), limit: limit}, nil}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() {
		c.Close()      //nolint:errcheck
		r.conn.Close() //nolint:errcheck
	})
	return c, r.conn
}

func TestMaxLineLenConn_ShortLinePassthrough(t *testing.T) {
	client, srv := newMaxLineLenTestConn(t, 1024)
	client.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	srv.SetDeadline(time.Now().Add(3 * time.Second))    //nolint:errcheck

	const line = "A001 CAPABILITY\r\n"
	fmt.Fprint(client, line)

	buf := make([]byte, 256)
	n, err := srv.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != line {
		t.Fatalf("expected %q, got %q", line, got)
	}
}

func TestMaxLineLenConn_LineTooLong_ClosesConn(t *testing.T) {
	const limit = 64
	client, srv := newMaxLineLenTestConn(t, limit)
	client.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	srv.SetDeadline(time.Now().Add(3 * time.Second))    //nolint:errcheck

	// Send a line that exceeds the limit.
	longLine := "A001 " + strings.Repeat("X", limit+1) + "\r\n"
	fmt.Fprint(client, longLine)

	buf := make([]byte, 256)
	_, err := srv.Read(buf)
	if err == nil {
		t.Fatal("expected error for line exceeding limit, got nil")
	}
}

func TestMaxLineLenConn_PendingBuffer(t *testing.T) {
	client, srv := newMaxLineLenTestConn(t, 1024)
	client.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	srv.SetDeadline(time.Now().Add(3 * time.Second))    //nolint:errcheck

	// Send two short lines back-to-back.
	fmt.Fprint(client, "A001 NOOP\r\nA002 LOGOUT\r\n")

	buf := make([]byte, 16) // small buffer to force pending usage
	n, err := srv.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("Read first: %v", err)
	}
	first := string(buf[:n])

	n2, err2 := srv.Read(buf)
	if err2 != nil && n2 == 0 {
		t.Fatalf("Read second: %v", err2)
	}
	combined := first + string(buf[:n2])
	if !strings.Contains(combined, "NOOP") {
		t.Fatalf("expected NOOP in combined reads, got %q", combined)
	}
}

func TestMaxLineLenListener_AcceptsAndWraps(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ml := &maxLineLenListener{Listener: ln, limit: 512}
	defer ml.Close() //nolint:errcheck

	ch := make(chan bool, 1)
	go func() {
		conn, err := ml.Accept()
		if err != nil {
			ch <- false
			return
		}
		_, ok := conn.(*maxLineLenConn)
		conn.Close() //nolint:errcheck
		ch <- ok
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close() //nolint:errcheck

	if ok := <-ch; !ok {
		t.Error("Accept() did not return *maxLineLenConn")
	}
}
