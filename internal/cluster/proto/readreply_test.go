package proto

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// newTestConn wraps one side of a net.Pipe as a *Conn without the dial-time
// handshake, so readReply / Lookup can be exercised directly.
func newTestConn(nc net.Conn) *Conn {
	return &Conn{conn: nc, rd: bufio.NewReaderSize(nc, maxLineLen)}
}

// TestLookup_SkipsInterleavedPushes: an unsolicited push (RING-CHANGE)
// arriving before the HOST reply must be skipped, not mistaken for it.
func TestLookup_SkipsInterleavedPushes(t *testing.T) {
	cliNC, dirNC := net.Pipe()
	defer cliNC.Close()
	defer dirNC.Close()
	c := newTestConn(cliNC)

	go func() {
		dir := bufio.NewReader(dirNC)
		_, _ = dir.ReadString('\n') // consume the LOOKUP request
		// two interleaved pushes, then the genuine reply
		_, _ = dirNC.Write([]byte("RING-CHANGE\t10.0.0.9\tup\timap\n"))
		_, _ = dirNC.Write([]byte("USER-KICKED\tbob@d.test\n"))
		_, _ = dirNC.Write([]byte("HOST\t1\t10.0.0.5\t10143\timap\n"))
	}()

	done := make(chan struct{})
	var res LookupResult
	var err error
	go func() { res, err = c.Lookup("1", "u@d.test", "imap", "imap"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Lookup blocked — interleaved push not skipped")
	}
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Addr != "10.0.0.5:10143" || res.Tag != "imap" {
		t.Fatalf("wrong reply: %+v", res)
	}
}

// TestReadReply_AnswersPing: a PING interleaved before the reply is answered
// with PONG and skipped.
func TestReadReply_AnswersPing(t *testing.T) {
	cliNC, dirNC := net.Pipe()
	defer cliNC.Close()
	defer dirNC.Close()
	c := newTestConn(cliNC)

	go func() {
		dir := bufio.NewReader(dirNC)
		_, _ = dir.ReadString('\n') // LOOKUP
		_, _ = dirNC.Write([]byte("PING\n"))
		// expect a PONG from the client before it accepts the reply
		line, _ := dir.ReadString('\n')
		if line != "PONG\n" {
			t.Errorf("want PONG after PING, got %q", line)
		}
		_, _ = dirNC.Write([]byte("HOST\t2\t10.0.0.6\t10143\t\n"))
	}()

	if _, err := c.Lookup("2", "u@d.test", "", "imap"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
}
