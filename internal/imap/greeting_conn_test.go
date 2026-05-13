package imap

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

func newGreetingTestConn(t *testing.T, greeting string) (client net.Conn, server *greetingConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		conn *greetingConn
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
		ch <- result{&greetingConn{Conn: c, replacement: greeting}, nil}
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

func TestGreetingConn_ReplacesGreeting(t *testing.T) {
	client, srv := newGreetingTestConn(t, "Yarilo ready")
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	cr := bufio.NewReader(client)
	srv.Write([]byte("* OK [CAPABILITY IMAP4rev1] IMAP server ready\r\n")) //nolint:errcheck

	line := readLineIMAP(t, cr)
	if !strings.Contains(line, "Yarilo ready") {
		t.Errorf("expected custom greeting, got %q", line)
	}
	if strings.Contains(line, "IMAP server ready") {
		t.Errorf("original greeting must be replaced, got %q", line)
	}
}

func TestGreetingConn_OnlyReplacesOnce(t *testing.T) {
	client, srv := newGreetingTestConn(t, "Custom")
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	cr := bufio.NewReader(client)

	srv.Write([]byte("* OK IMAP server ready\r\n")) //nolint:errcheck
	srv.Write([]byte("* OK IMAP server ready\r\n")) //nolint:errcheck

	first := readLineIMAP(t, cr)
	second := readLineIMAP(t, cr)

	if !strings.Contains(first, "Custom") {
		t.Errorf("first write must use custom greeting, got %q", first)
	}
	if !strings.Contains(second, "IMAP server ready") {
		t.Errorf("second write must not be modified, got %q", second)
	}
}

func TestGreetingConn_NonGreetingPassthrough(t *testing.T) {
	client, srv := newGreetingTestConn(t, "Custom")
	d := time.Now().Add(5 * time.Second)
	client.SetDeadline(d) //nolint:errcheck
	srv.SetDeadline(d)    //nolint:errcheck

	cr := bufio.NewReader(client)
	srv.Write([]byte("A1 OK logged in\r\n")) //nolint:errcheck

	line := readLineIMAP(t, cr)
	if line != "A1 OK logged in" {
		t.Errorf("non-greeting write must pass through unchanged, got %q", line)
	}
}

func TestGreetingListener_AcceptsAndWraps(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &greetingListener{Listener: ln, greeting: "Hello"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := wrapped.Accept()
		if err != nil {
			return
		}
		if _, ok := conn.(*greetingConn); !ok {
			t.Error("Accept must return *greetingConn")
		}
		conn.Close()
	}()

	c, _ := net.Dial("tcp", ln.Addr().String())
	<-done
	c.Close()
	ln.Close()
}
