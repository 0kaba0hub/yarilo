package locks_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// refusingThenAccepting is a listener that is not there for the first attempts
// and is there afterwards -- a lock service a few seconds behind the component
// that needs it, which is ordinary in a cluster (#1350).
func refusingThenAccepting(t *testing.T, refuseFor time.Duration) (addr string, dialled *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = ln.Addr().String()
	// Close it so the first dials are refused, then re-listen on the same port.
	ln.Close() //nolint:errcheck
	var count int32
	go func() {
		time.Sleep(refuseFor)
		l2, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return
		}
		t.Cleanup(func() { l2.Close() }) //nolint:errcheck
		for {
			conn, aerr := l2.Accept()
			if aerr != nil {
				return
			}
			atomic.AddInt32(&count, 1)
			go func(c net.Conn) {
				rd := bufio.NewReader(c)
				line, rerr := rd.ReadString('\n')
				if rerr != nil || !strings.HasPrefix(line, "VERSION") {
					c.Close() //nolint:errcheck
					return
				}
				_, _ = c.Write([]byte("VERSION\t1\tOK\n"))
			}(conn)
		}
	}()
	return addr, &count
}

// A component that starts before the lock service must wait for it, not exit.
// Exiting costs a restart, and spends the RESTARTS counter every rollout window
// is judged by.
func TestClientWaitsForAServiceThatIsLate(t *testing.T) {
	addr, dialled := refusingThenAccepting(t, 300*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := locks.NewClientWaiting(ctx, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("the client gave up on a service that was 300ms late: %v", err)
	}
	defer client.Close() //nolint:errcheck
	if atomic.LoadInt32(dialled) == 0 {
		t.Error("no connection was made; the wait returned without connecting")
	}
}

// The mirror, and the row that keeps the change honest: an endpoint that never
// answers must still fail, with the reason, rather than retry for ever inside a
// pod that looks healthy.
func TestClientStillGivesUpOnAnEndpointThatNeverAnswers(t *testing.T) {
	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = locks.NewClientWaiting(ctx, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}, 400*time.Millisecond)
	if err == nil {
		t.Fatal("a dead endpoint reported a working client")
	}
	if !strings.Contains(err.Error(), "not reachable after") {
		t.Errorf("the failure does not say what happened: %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %s for a bounded window of 400ms", waited)
	}
}

// wait <= 0 is exactly NewClient, so a deployment can turn the behaviour off.
func TestZeroWaitDoesNotRetry(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() //nolint:errcheck

	start := time.Now()
	if _, err := locks.NewClientWaiting(context.Background(), func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}, 0); err == nil {
		t.Fatal("a dead endpoint reported a working client")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("waited %s with waiting disabled", waited)
	}
}
