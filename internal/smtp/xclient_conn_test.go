package smtp

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// newTestConn creates a real TCP connection pair. server is wrapped in
// xclientConn. Uses TCP so OS send buffers avoid deadlocks.
func newTestConn(t *testing.T) (client net.Conn, server *xclientConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		conn *xclientConn
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
		ch <- result{newXClientConn(c), nil}
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

func deadline(d time.Duration) time.Time { return time.Now().Add(d) }

// readLine reads one CRLF-terminated line from r.
func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// ---- Read side -----------------------------------------------------------

func TestXClientConn_RemoteAddrDefault(t *testing.T) {
	_, srv := newTestConn(t)
	if srv.RemoteAddr() == nil {
		t.Fatal("RemoteAddr should not be nil before XCLIENT")
	}
}

func TestXClientConn_UpdatesRemoteAddr(t *testing.T) {
	client, srv := newTestConn(t)
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	// Send XCLIENT then a regular line.
	fmt.Fprintf(client, "XCLIENT ADDR=1.2.3.4 NAME=[UNAVAILABLE]\r\n")

	// srv.Read must consume the XCLIENT line internally (respond 220, not forward it).
	buf := make([]byte, 256)
	// Give the server time to process. Then send a passthrough line.
	fmt.Fprintf(client, "EHLO relay.example.com\r\n")

	n, err := srv.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	line := strings.TrimRight(string(buf[:n]), "\r\n")
	if !strings.Contains(line, "EHLO") {
		t.Fatalf("expected EHLO, got %q", line)
	}

	ip := srv.RemoteAddr().(*net.TCPAddr).IP
	if !ip.Equal(net.ParseIP("1.2.3.4")) {
		t.Fatalf("expected 1.2.3.4, got %v", ip)
	}
}

func TestXClientConn_XClientResponds220(t *testing.T) {
	client, srv := newTestConn(t)
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	fmt.Fprintf(client, "XCLIENT ADDR=2.2.2.2\r\n")
	fmt.Fprintf(client, "QUIT\r\n")

	// srv.Read must be driven to trigger XCLIENT interception + 220 write.
	go func() {
		buf := make([]byte, 256)
		srv.Read(buf) //nolint:errcheck
	}()

	// Client must receive "220 2.0.0 OK" response.
	cr := bufio.NewReader(client)
	line := readLine(t, cr)
	if !strings.HasPrefix(line, "220") {
		t.Fatalf("expected 220 response to XCLIENT, got %q", line)
	}
}

func TestXClientConn_NonXClientPassthrough(t *testing.T) {
	client, srv := newTestConn(t)
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	const line = "EHLO plain.example.com\r\n"
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

func TestXClientConn_CaseInsensitive(t *testing.T) {
	client, srv := newTestConn(t)
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	// lowercase xclient
	fmt.Fprintf(client, "xclient addr=5.6.7.8\r\n")
	fmt.Fprintf(client, "NOOP\r\n")

	buf := make([]byte, 256)
	n, _ := srv.Read(buf)
	line := strings.TrimRight(string(buf[:n]), "\r\n")
	if !strings.Contains(line, "NOOP") {
		t.Fatalf("expected NOOP after lowercase xclient, got %q", line)
	}
	ip := srv.RemoteAddr().(*net.TCPAddr).IP
	if !ip.Equal(net.ParseIP("5.6.7.8")) {
		t.Fatalf("expected 5.6.7.8 after lowercase xclient, got %v", ip)
	}
}

// ---- Write side ----------------------------------------------------------

func TestXClientConn_InjectsCapIntoEHLO(t *testing.T) {
	client, srv := newTestConn(t)
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	// Simulate go-smtp writing EHLO response line by line.
	go func() {
		srv.Write([]byte("250-yarilo.example.com\r\n")) //nolint:errcheck
		srv.Write([]byte("250-PIPELINING\r\n"))          //nolint:errcheck
		srv.Write([]byte("250 SIZE 41943040\r\n"))       //nolint:errcheck
	}()

	cr := bufio.NewReader(client)
	var lines []string
	for {
		line, err := cr.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			lines = append(lines, line)
		}
		if err != nil || strings.HasPrefix(line, "250 ") {
			break
		}
	}

	full := strings.Join(lines, "\n")
	if !strings.Contains(full, "250-XCLIENT ADDR NAME") {
		t.Fatalf("XCLIENT cap not injected into EHLO response:\n%s", full)
	}
	xclientIdx := strings.Index(full, "250-XCLIENT")
	finalIdx := strings.Index(full, "250 SIZE")
	if xclientIdx < 0 || finalIdx < 0 || xclientIdx > finalIdx {
		t.Fatalf("XCLIENT cap not before final 250 line:\n%s", full)
	}
}

func TestXClientConn_NoInjectOnSingleLine250(t *testing.T) {
	client, srv := newTestConn(t)
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	go srv.Write([]byte("250 2.0.0 OK\r\n")) //nolint:errcheck

	buf := make([]byte, 64)
	n, _ := client.Read(buf)
	resp := string(buf[:n])
	if strings.Contains(resp, "XCLIENT") {
		t.Fatalf("unexpected XCLIENT injection in single-line 250: %q", resp)
	}
}

func TestXClientConn_NoInjectOnNon250(t *testing.T) {
	client, srv := newTestConn(t)
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	go srv.Write([]byte("220 yarilo.example.com ESMTP\r\n")) //nolint:errcheck

	buf := make([]byte, 64)
	n, _ := client.Read(buf)
	resp := string(buf[:n])
	if strings.Contains(resp, "XCLIENT") {
		t.Fatalf("unexpected XCLIENT injection in 220 banner: %q", resp)
	}
}

// ---- xclientListener -----------------------------------------------------

func TestXClientListener_AcceptsAndWraps(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	xl := &xclientListener{ln}
	defer xl.Close() //nolint:errcheck

	ch := make(chan bool, 1)
	go func() {
		conn, err := xl.Accept()
		if err != nil {
			ch <- false
			return
		}
		_, ok := conn.(*xclientConn)
		conn.Close() //nolint:errcheck
		ch <- ok
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close() //nolint:errcheck

	if ok := <-ch; !ok {
		t.Error("Accept() did not return *xclientConn")
	}
}
