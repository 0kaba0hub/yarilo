package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

var loopbackNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("127.0.0.0/8")
	return n
}()

func deadline(d time.Duration) time.Time { return time.Now().Add(d) }

func readLineIMAP(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func newXClientImapTestConn(t *testing.T, trustedNets []*net.IPNet) (client net.Conn, server *xclientImapConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		conn *xclientImapConn
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
		ch <- result{newXClientImapConn(c, trustedNets), nil}
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

// ---- Read side (XCLIENT command interception) --------------------------------

func TestXClientImapConn_RemoteAddrDefault(t *testing.T) {
	_, srv := newXClientImapTestConn(t, []*net.IPNet{loopbackNet})
	if srv.RemoteAddr() == nil {
		t.Fatal("RemoteAddr should not be nil before XCLIENT")
	}
}

func TestXClientImapConn_UpdatesRemoteAddr(t *testing.T) {
	client, srv := newXClientImapTestConn(t, []*net.IPNet{loopbackNet})
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	// Send tagged XCLIENT then a regular IMAP command.
	fmt.Fprintf(client, "A001 XCLIENT ADDR=1.2.3.4\r\n")
	fmt.Fprintf(client, "A002 CAPABILITY\r\n")

	buf := make([]byte, 256)
	n, err := srv.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	line := strings.TrimRight(string(buf[:n]), "\r\n")
	if !strings.Contains(line, "CAPABILITY") {
		t.Fatalf("expected CAPABILITY passthrough, got %q", line)
	}
	ip := srv.RemoteAddr().(*net.TCPAddr).IP
	if !ip.Equal(net.ParseIP("1.2.3.4")) {
		t.Fatalf("expected 1.2.3.4, got %v", ip)
	}
}

func TestXClientImapConn_RespondsTaggedOK(t *testing.T) {
	client, srv := newXClientImapTestConn(t, []*net.IPNet{loopbackNet})
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	fmt.Fprintf(client, "T1 XCLIENT ADDR=2.2.2.2\r\n")
	fmt.Fprintf(client, "T2 NOOP\r\n")

	go func() {
		buf := make([]byte, 256)
		srv.Read(buf) //nolint:errcheck
	}()

	cr := bufio.NewReader(client)
	resp := readLineIMAP(t, cr)
	if !strings.HasPrefix(resp, "T1 OK XCLIENT") {
		t.Fatalf("expected 'T1 OK XCLIENT', got %q", resp)
	}
}

func TestXClientImapConn_NonXClientPassthrough(t *testing.T) {
	client, srv := newXClientImapTestConn(t, []*net.IPNet{loopbackNet})
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

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

func TestXClientImapConn_UntrustedNet_Ignored(t *testing.T) {
	_, private10, _ := net.ParseCIDR("10.0.0.0/8")
	client, srv := newXClientImapTestConn(t, []*net.IPNet{private10})
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	origAddr := srv.RemoteAddr().String()

	fmt.Fprintf(client, "A001 XCLIENT ADDR=9.9.9.9\r\n")
	fmt.Fprintf(client, "A002 NOOP\r\n")

	buf := make([]byte, 256)
	n, err := srv.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	line := strings.TrimRight(string(buf[:n]), "\r\n")
	if !strings.Contains(line, "NOOP") {
		t.Fatalf("expected NOOP passthrough, got %q", line)
	}
	if got := srv.RemoteAddr().String(); got != origAddr {
		t.Fatalf("remoteAddr must not change for untrusted peer: was %q, got %q", origAddr, got)
	}
}

// ---- Write side (CAPABILITY injection) --------------------------------------

func TestXClientImapConn_InjectsXClientInCapability(t *testing.T) {
	client, srv := newXClientImapTestConn(t, []*net.IPNet{loopbackNet})
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	go srv.Write([]byte("* CAPABILITY IMAP4rev2 IMAP4rev1 IDLE\r\n")) //nolint:errcheck

	cr := bufio.NewReader(client)
	line := readLineIMAP(t, cr)
	if !strings.Contains(line, "XCLIENT") {
		t.Fatalf("XCLIENT must appear in CAPABILITY for trusted peer, got %q", line)
	}
}

func TestXClientImapConn_NoInjectForNonCapability(t *testing.T) {
	client, srv := newXClientImapTestConn(t, []*net.IPNet{loopbackNet})
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	go srv.Write([]byte("* OK yarilo IMAP server ready\r\n")) //nolint:errcheck

	cr := bufio.NewReader(client)
	line := readLineIMAP(t, cr)
	if strings.Contains(line, "XCLIENT") {
		t.Fatalf("XCLIENT must not appear in greeting, got %q", line)
	}
}

func TestXClientImapConn_NoInjectForUntrustedPeer(t *testing.T) {
	_, private10, _ := net.ParseCIDR("10.0.0.0/8")
	client, srv := newXClientImapTestConn(t, []*net.IPNet{private10})
	client.SetDeadline(deadline(5 * time.Second)) //nolint:errcheck
	srv.SetDeadline(deadline(5 * time.Second))    //nolint:errcheck

	go srv.Write([]byte("* CAPABILITY IMAP4rev2 IMAP4rev1\r\n")) //nolint:errcheck

	cr := bufio.NewReader(client)
	line := readLineIMAP(t, cr)
	if strings.Contains(line, "XCLIENT") {
		t.Fatalf("XCLIENT must not appear for untrusted peer, got %q", line)
	}
}

// ---- xclientImapListener ----------------------------------------------------

func TestXClientImapListener_AcceptsAndWraps(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	xl := &xclientImapListener{Listener: ln}
	defer xl.Close() //nolint:errcheck

	ch := make(chan bool, 1)
	go func() {
		conn, err := xl.Accept()
		if err != nil {
			ch <- false
			return
		}
		_, ok := conn.(*xclientImapConn)
		conn.Close() //nolint:errcheck
		ch <- ok
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close() //nolint:errcheck

	if ok := <-ch; !ok {
		t.Error("Accept() did not return *xclientImapConn")
	}
}
