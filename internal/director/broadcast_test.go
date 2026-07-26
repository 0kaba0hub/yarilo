package director

import (
	"net"
	"testing"
	"time"
)

// TestBroadcast_SlowClientDoesNotStall guards #704: a client whose socket never
// drains must not block the broadcast fan-out — the per-write deadline bounds it.
func TestBroadcast_SlowClientDoesNotStall(t *testing.T) {
	s := NewWithOptions(Options{WriteTimeout: 100 * time.Millisecond})

	// net.Pipe is unbuffered: a write blocks until the other side reads. We
	// never read, so without a write deadline WriteLine would block forever.
	stuckNC, peerNC := net.Pipe()
	defer stuckNC.Close()
	defer peerNC.Close()
	stuck := &client{conn: stuckNC, writeTimeout: s.opts.writeTimeout(), pongCh: make(chan struct{}, 1)}
	s.addClient(stuck)

	done := make(chan struct{})
	go func() { s.broadcast("RING-CHANGE\t10.0.0.1\tup\t", nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast stalled on a slow client — write deadline not applied")
	}
}

// TestWriteTimeout_Config: default 10s, negative disables.
func TestWriteTimeout_Config(t *testing.T) {
	if got := (&Options{}).writeTimeout(); got != 10*time.Second {
		t.Errorf("default writeTimeout = %v, want 10s", got)
	}
	if got := (&Options{WriteTimeout: -1}).writeTimeout(); got != 0 {
		t.Errorf("negative writeTimeout = %v, want 0 (disabled)", got)
	}
	if got := (&Options{WriteTimeout: 3 * time.Second}).writeTimeout(); got != 3*time.Second {
		t.Errorf("explicit writeTimeout = %v, want 3s", got)
	}
}
