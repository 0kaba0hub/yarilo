package locks_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// A lock call made while the service goes away must return an error. It used to
// panic instead: the deferred deadline reset re-read slot.conn at return, and a
// failed reconnect had already cleared it, so the nil dereference happened on
// the one path that runs when the service is unreachable (#1336).
//
// A panic there is not one failed call. It takes down the process holding every
// other session on that pod, and it happens during an ordinary rolling upgrade
// of yarilo-locks.
func TestLockCallSurvivesTheServiceVanishing(t *testing.T) {
	// A server that completes the handshake and then goes away, which is what a
	// pod being rescheduled looks like from here: the connection is good, the
	// next command is not.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			// Answer the version handshake, then hand the connection over.
			rd := bufio.NewReader(conn)
			line, rerr := rd.ReadString('\n')
			if rerr != nil || !strings.HasPrefix(line, "VERSION") {
				conn.Close() //nolint:errcheck
				continue
			}
			if _, werr := conn.Write([]byte("VERSION\t1\tOK\n")); werr != nil {
				conn.Close() //nolint:errcheck
				continue
			}
			accepted <- conn
		}
	}()

	client, err := locks.NewClient(context.Background(), func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close() //nolint:errcheck

	// Force the pool to hold a live, handshaken connection.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Lock(locks.WithSite(ctx, "write"), "warmup", "test.bin/1/alice@example.com/sess1", time.Second); err == nil {
		// The stub answers nothing after the handshake, so the warm-up is
		// expected to fail on read; what matters is that a connection exists.
		t.Log("warm-up unexpectedly succeeded")
	}

	// Now the service disappears: listener closed, every accepted connection
	// dropped. The next call finds a dead connection and cannot reconnect.
	ln.Close() //nolint:errcheck
	close(accepted)
	for conn := range accepted {
		conn.Close() //nolint:errcheck
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	// The assertion is that this returns at all. A panic here fails the test by
	// killing it, which is the honest shape: the defect is not a wrong value.
	if _, err := client.Lock(locks.WithSite(ctx2, "write"), "mailbox/u1", "test.bin/1/alice@example.com/sess1", time.Second); err == nil {
		t.Error("locking against a service that is gone reported success")
	}
}
