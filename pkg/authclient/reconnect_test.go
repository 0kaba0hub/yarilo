package authclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// #1410: backend-api kept writing into a socket the kernel had already given
// up on, failing every userdb lookup with the same local port until the pod was
// restarted, while IMAP and JMAP served the same user without trouble.
//
// The decision is made here, so this is where it is tested: "timeout" covers
// both our own elapsed budget and the kernel exhausting its retransmissions,
// and only the second one means the connection is gone.
func TestReconnectDecision(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			// What the field failure looked like: write: connection timed out.
			name: "the kernel gave up retransmitting",
			err: &net.OpError{Op: "write", Net: "tcp",
				Err: os.NewSyscallError("write", syscall.ETIMEDOUT)},
			want: true,
		},
		{
			// Our own request budget. The connection may be healthy and the
			// next request will use it; redialing here reconnects on every
			// slow answer. Pinned rather than fixed: this shape already failed
			// the Timeout() test, and the mutation that removed a dedicated
			// case for it changed nothing -- which is how the dedicated case
			// was found to be decoration and dropped.
			name: "our deadline elapsed",
			err: &net.OpError{Op: "read", Net: "tcp",
				Err: os.ErrDeadlineExceeded},
			want: false,
		},
		{
			name: "the caller cancelled",
			err:  fmt.Errorf("authclient: read: %w", context.Canceled),
			want: false,
		},
		{
			name: "the peer closed",
			err:  fmt.Errorf("authclient: read: %w", io.EOF),
			want: true,
		},
		{
			name: "connection reset",
			err: &net.OpError{Op: "read", Net: "tcp",
				Err: os.NewSyscallError("read", syscall.ECONNRESET)},
			want: true,
		},
		{
			// An answer we could not parse is not a transport failure, and
			// redialing would throw away a working connection on every one.
			name: "a malformed response",
			err:  errors.New("authclient: response for unexpected id: \"USER\\t7\""),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConnectionError(tt.err); got != tt.want {
				t.Errorf("isConnectionError = %v, want %v (err: %v)", got, tt.want, tt.err)
			}
		})
	}
}

// The wedge, end to end: a connection whose peer is gone must not fail every
// later request. The kernel timeout cannot be staged from a test, so this
// stages the other dead-socket shape -- the peer closing -- and pins that a
// second lookup succeeds on a fresh connection rather than inheriting the
// corpse.
func TestALookupSurvivesTheConnectionGoingAway(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serve := func(conn net.Conn, answer bool) {
		defer conn.Close()
		_, _ = conn.Write([]byte("VERSION\tyarilo-auth-master\t1\t0\nDONE\n"))
		buf := make([]byte, 256)
		n, err := conn.Read(buf)
		if err != nil || !answer {
			return // first connection: die instead of answering
		}
		// The id is the second TAB field of the request.
		var verb, id string
		_, _ = fmt.Sscanf(string(buf[:n]), "%s\t%s", &verb, &id)
		_, _ = conn.Write([]byte("NOTFOUND\t1\n"))
	}

	go func() {
		first, err := ln.Accept()
		if err != nil {
			return
		}
		serve(first, false)
		second, err := ln.Accept()
		if err != nil {
			return
		}
		serve(second, true)
	}()

	c, err := Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	ui, err := c.Userdb(context.Background(), "u@example.com")
	if err != nil {
		t.Fatalf("the lookup did not recover from a dead connection: %v", err)
	}
	if ui != nil {
		t.Errorf("NOTFOUND produced a user: %+v", ui)
	}
}
