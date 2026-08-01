package warden

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPoolReusesConnectionsAcrossSessions is the #878 acceptance test for warden:
// the number of connections must follow the pool size, not the session count.
func TestPoolReusesConnectionsAcrossSessions(t *testing.T) {
	srv, addr := startTestServer(t, 0)
	_ = srv

	const (
		size     = 2
		sessions = 50
	)
	p := NewPool(addr, nil, size, time.Second)
	defer p.Close()

	for i := range sessions {
		id := "s" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('0'+i%10))
		if err := p.Connect(id, "u@d", "10.0.0.1", "imap"); err != nil {
			t.Fatalf("Connect %s: %v", id, err)
		}
	}

	if got := p.Size(); got != size {
		t.Fatalf("pool size = %d, want %d", got, size)
	}
	// Every session registered over at most `size` connections.
	if n := srv.acceptCount(); n > size {
		t.Fatalf("server accepted %d connections for %d sessions, want <= %d", n, sessions, size)
	}
}

func TestPoolConcurrentUseIsSafe(t *testing.T) {
	srv, addr := startTestServer(t, 0)
	p := NewPool(addr, nil, 3, time.Second)
	defer p.Close()

	const workers = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sess" + string(rune('A'+i%26)) + string(rune('0'+i/26))
			if err := p.Connect(id, "u@d", "10.0.0.1", "imap"); err != nil {
				errs <- err
				return
			}
			if _, err := p.Heartbeat(id); err != nil {
				errs <- err
				return
			}
			if err := p.Disconnect(id, "u@d", "10.0.0.1", "imap"); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent pool use: %v", err)
	}
	if n := srv.acceptCount(); n > 3 {
		t.Fatalf("accepted %d connections, want <= pool size 3", n)
	}
}

// TestPoolPassesThroughTooManyConns covers the distinction the retry logic
// depends on: ErrTooManyConns is a protocol answer and must reach the caller
// unchanged, never be mistaken for a transport failure and retried.
func TestPoolPassesThroughTooManyConns(t *testing.T) {
	_, addr := startTestServer(t, 1)
	p := NewPool(addr, nil, 2, time.Second)
	defer p.Close()

	if err := p.Connect("s1", "u@d", "10.0.0.1", "imap"); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	err := p.Connect("s2", "u@d", "10.0.0.1", "imap")
	if err != ErrTooManyConns {
		t.Fatalf("second Connect = %v, want ErrTooManyConns", err)
	}
}

// TestPoolRedialsAfterConnectionLoss covers the recovery path: a dead pooled
// connection is replaced and the operation retried, because every warden command
// is idempotent in its session id.
func TestPoolRedialsAfterConnectionLoss(t *testing.T) {
	srv, addr := startTestServer(t, 0)
	p := NewPool(addr, nil, 1, time.Second)
	defer p.Close()

	if err := p.Connect("s1", "u@d", "10.0.0.1", "imap"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	before := srv.acceptCount()

	// Kill the pooled connection from underneath the pool.
	p.conns[0].mu.Lock()
	p.conns[0].c.Close()
	p.conns[0].mu.Unlock()

	if _, err := p.Heartbeat("s1"); err != nil {
		t.Fatalf("Heartbeat after connection loss: %v", err)
	}
	if after := srv.acceptCount(); after <= before {
		t.Fatalf("accepts %d → %d, want a redial", before, after)
	}
}

func TestPoolHeartbeatLoopRejectsZeroInterval(t *testing.T) {
	_, addr := startTestServer(t, 0)
	p := NewPool(addr, nil, 1, time.Second)
	defer p.Close()

	if err := p.HeartbeatLoop(context.Background(), "s1", 0, nil); err == nil {
		t.Fatal("HeartbeatLoop with interval 0: want error")
	}
}

func TestPoolClosedRejectsUse(t *testing.T) {
	_, addr := startTestServer(t, 0)
	p := NewPool(addr, nil, 1, time.Second)
	p.Close()
	p.Close() // idempotent

	if err := p.Connect("s1", "u@d", "10.0.0.1", "imap"); err == nil {
		t.Fatal("Connect on a closed pool: want error")
	}
}

func TestNewPoolSizeDefaults(t *testing.T) {
	tests := []struct {
		name string
		size int
		want int
	}{
		{"explicit", 3, 3},
		{"zero", 0, DefaultPoolSize},
		{"negative", -2, DefaultPoolSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPool("127.0.0.1:1", nil, tc.size, time.Second)
			defer p.Close()
			if got := p.Size(); got != tc.want {
				t.Fatalf("Size() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPoolPenaltyRedialsAfterConnectionLoss is the #946 regression: yarilo-auth's
// penalty ops must survive an warden restart. Over the pool, a dead connection is
// redialed and the op retried, where a raw single Conn failed every penalty op
// forever (broken pipe) until auth itself was restarted — silently disabling the
// tarpit.
func TestPoolPenaltyRedialsAfterConnectionLoss(t *testing.T) {
	srv, addr := startTestServer(t, 0)
	p := NewPool(addr, nil, 1, time.Second)
	defer p.Close()

	if err := p.PenaltyUpdate("1.2.3.4", 3); err != nil {
		t.Fatalf("PenaltyUpdate: %v", err)
	}
	if n, err := p.PenaltyLookup("1.2.3.4"); err != nil || n != 3 {
		t.Fatalf("PenaltyLookup = (%d,%v), want (3,nil)", n, err)
	}
	before := srv.acceptCount()

	// Kill the pooled connection from underneath the pool (an warden restart).
	p.conns[0].mu.Lock()
	p.conns[0].c.Close()
	p.conns[0].mu.Unlock()

	// The next penalty op must reconnect and succeed, not fail with broken pipe.
	if n, err := p.PenaltyLookup("1.2.3.4"); err != nil || n != 3 {
		t.Fatalf("PenaltyLookup after connection loss = (%d,%v), want (3,nil)", n, err)
	}
	if after := srv.acceptCount(); after <= before {
		t.Fatalf("accepts %d → %d, want a redial", before, after)
	}
}
