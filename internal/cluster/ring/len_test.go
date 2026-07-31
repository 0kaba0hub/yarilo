package ring

import (
	"testing"
	"time"
)

// TestLenCountsBackends: Len reflects the registered backend count.
func TestLenCountsBackends(t *testing.T) {
	r := New(MustParseHashFormat("%u"))
	if got := r.Len(); got != 0 {
		t.Fatalf("empty ring Len = %d, want 0", got)
	}
	r.AddBackend(&Backend{IP: "10.0.0.1", Port: 143, Up: true})
	r.AddBackend(&Backend{IP: "10.0.0.2", Port: 143, Up: true})
	if got := r.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// TestLenBlocksUnderWriteHold is the property the #904 liveness probe relies on:
// Len takes the same r.mu the ring's writers hold, so a writer that never
// releases the lock — the wedged-ring deadlock — blocks the probe, which the
// watchdog then observes as a failure. Held with the real write-lock, not a
// mock, so the block is genuinely on the mutex.
func TestLenBlocksUnderWriteHold(t *testing.T) {
	r := New(MustParseHashFormat("%u"))

	r.mu.Lock() // simulate a handler wedged under the ring write-lock
	done := make(chan int, 1)
	go func() { done <- r.Len() }()

	select {
	case <-done:
		r.mu.Unlock()
		t.Fatal("Len returned while the write-lock was held — the probe cannot detect a wedged ring")
	case <-time.After(50 * time.Millisecond):
		// Expected: the probe is blocked on the mutex.
	}

	r.mu.Unlock()
	select {
	case <-done: // Len completes once the lock is released.
	case <-time.After(time.Second):
		t.Fatal("Len did not complete after the write-lock was released")
	}
}
