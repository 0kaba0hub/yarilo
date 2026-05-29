package integration_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

// TestEmitArrivesAtSubscriber wires the pkg/locks EVENT channel end-to-end
// through an embedded yarilo-locks server: one Locker client publishes an
// `delivered` event, another (on a separate connection — i.e. a separate
// "pod") subscribed to the same mailbox key receives it. Phase 2.4 relies
// on this contract to wake up IMAP IDLE sessions across pods.
func TestEmitArrivesAtSubscriber(t *testing.T) {
	// Embedded server — Unix socket + in-memory backend.
	sock := filepath.Join(mustShortTempDir(t), "l.sock")
	backend := locks.NewMemoryBackend(locks.WithSweepInterval(5 * time.Millisecond))
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, ln)
		close(done)
	}()
	if !waitDialUnix(sock) {
		t.Fatal("server did not start")
	}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		_ = backend.Close()
		<-done
	})

	dial := locks.DialUnix(sock)

	// Subscriber ("pod A — IMAP IDLE").
	subscriber, err := locks.NewClient(context.Background(), dial)
	if err != nil {
		t.Fatalf("dial subscriber: %v", err)
	}
	t.Cleanup(func() { _ = subscriber.Close() })

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	resource := locks.MailboxKey("alice@example.com", "INBOX")
	events, err := subscriber.Subscribe(subCtx, resource)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Give the server a brief moment to register the subscription before
	// publishing. Without this the event may race the SUBSCRIBE registration.
	time.Sleep(50 * time.Millisecond)

	// Publisher ("pod B — LMTP delivery").
	publisher, err := locks.NewClient(context.Background(), dial)
	if err != nil {
		t.Fatalf("dial publisher: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	want := uint32(42)
	if err := publisher.Emit(context.Background(), resource, locks.EventDelivered, strconv.FormatUint(uint64(want), 10)); err != nil {
		t.Fatalf("emit: %v", err)
	}

	select {
	case evt, ok := <-events:
		if !ok {
			t.Fatal("event channel closed before delivery")
		}
		if evt.Type != locks.EventDelivered {
			t.Fatalf("type: want %q, got %q", locks.EventDelivered, evt.Type)
		}
		if evt.Resource != resource {
			t.Fatalf("resource: want %q, got %q", resource, evt.Resource)
		}
		if evt.Payload != "42" {
			t.Fatalf("payload: want %q, got %q", "42", evt.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func mustShortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yl-evt")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitDialUnix(path string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
