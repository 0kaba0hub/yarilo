package login

import (
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns two ends of a real TCP connection. net.Pipe cannot be used
// here: it has no CloseWrite, so halfClose is a no-op on it and the backend
// never sees the client's EOF -- the test would then measure the grace in
// every case, including the clean one. The transport has to support
// half-close, because half-close is what the code under test relies on.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a := <-ch
	if a.err != nil {
		t.Fatal(a.err)
	}
	t.Cleanup(func() { dialed.Close(); a.c.Close() })
	return dialed, a.c
}

// #1404: a session whose client is gone kept its backend leg, and with it the
// proxy, the session record, and every director's copy of that record. On IMAP
// the backend's read timeout eventually ended it; on ManageSieve nothing did,
// so a smoke suite left two behind every run and they are what the kill
// confirm waits on.
//
// The backend here answers nothing after the client leaves -- exactly the
// ManageSieve server's behaviour -- so only the proxy itself can end this.
func TestProxyEndsWhenTheClientIsGoneAndTheBackendIsSilent(t *testing.T) {
	clientEnd, clientSide := tcpPair(t)
	backendSide, backendEnd := tcpPair(t)

	// The backend reads and never writes.
	go io.Copy(io.Discard, backendEnd) //nolint:errcheck

	done := make(chan struct{})
	go func() {
		biProxy(clientSide, clientSide, backendSide, backendSide, func() { backendSide.Close() })
		close(done)
	}()

	// The client says its piece and leaves.
	if _, err := clientEnd.Write([]byte("PUTSCRIPT\r\n")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	clientEnd.Close()

	select {
	case <-done:
	case <-time.After(clientGoneGrace + 5*time.Second):
		t.Fatal("the proxy is still running after the client left: the session record is never released")
	}
}

// A backend that closes on its own must still end the session promptly -- the
// grace is for the case where it does NOT, and must not be paid otherwise.
func TestProxyEndsAtOnceWhenTheBackendCloses(t *testing.T) {
	clientEnd, clientSide := tcpPair(t)
	backendSide, backendEnd := tcpPair(t)

	go func() {
		io.Copy(io.Discard, backendEnd) //nolint:errcheck
		backendEnd.Close()
	}()

	done := make(chan struct{})
	go func() {
		biProxy(clientSide, clientSide, backendSide, backendSide, func() { backendSide.Close() })
		close(done)
	}()

	clientEnd.Write([]byte("LOGOUT\r\n")) //nolint:errcheck
	clientEnd.Close()

	start := time.Now()
	select {
	case <-done:
	case <-time.After(clientGoneGrace + 5*time.Second):
		t.Fatal("the proxy did not end after both sides finished")
	}
	if waited := time.Since(start); waited >= clientGoneGrace {
		t.Errorf("a clean close waited %v for the grace; the grace is for a backend that does not answer", waited)
	}
}

// Data still in flight when the client's last byte arrives must reach the
// client leg before the session is taken down -- the grace exists for that.
func TestProxyDeliversInFlightDataBeforeClosing(t *testing.T) {
	clientEnd, clientSide := tcpPair(t)
	backendSide, backendEnd := tcpPair(t)

	go func() {
		buf := make([]byte, 64)
		backendEnd.Read(buf) //nolint:errcheck
		time.Sleep(200 * time.Millisecond)
		backendEnd.Write([]byte("OK late\r\n")) //nolint:errcheck
	}()

	go biProxy(clientSide, clientSide, backendSide, backendSide, func() { backendSide.Close() })

	clientEnd.Write([]byte("GETSCRIPT\r\n")) //nolint:errcheck
	// The client is done sending and waits for the answer: its write side
	// closes, which ends the client-to-backend copy and starts the grace. With
	// no grace the reply still in flight would be cut off here.
	clientEnd.(*net.TCPConn).CloseWrite() //nolint:errcheck

	got := make([]byte, 32)
	clientEnd.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	n, err := clientEnd.Read(got)
	if err != nil {
		t.Fatalf("the late reply never reached the client: %v", err)
	}
	if string(got[:n]) != "OK late\r\n" {
		t.Errorf("client got %q, want the backend's reply", got[:n])
	}
}
