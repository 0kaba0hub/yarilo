package connlimit

import (
	"sync"
	"testing"
)

func TestLimiter_Basic(t *testing.T) {
	l := New(2)

	if !l.Acquire("alice", "1.2.3.4") {
		t.Fatal("first acquire must succeed")
	}
	if !l.Acquire("alice", "1.2.3.4") {
		t.Fatal("second acquire must succeed (limit=2)")
	}
	if l.Acquire("alice", "1.2.3.4") {
		t.Fatal("third acquire must fail (limit=2)")
	}

	l.Release("alice", "1.2.3.4")
	if !l.Acquire("alice", "1.2.3.4") {
		t.Fatal("acquire after release must succeed")
	}
}

func TestLimiter_DifferentIPs(t *testing.T) {
	l := New(1)

	if !l.Acquire("alice", "1.1.1.1") {
		t.Fatal("alice@1.1.1.1 must succeed")
	}
	if !l.Acquire("alice", "2.2.2.2") {
		t.Fatal("alice@2.2.2.2 is a different key, must succeed")
	}
	if l.Acquire("alice", "1.1.1.1") {
		t.Fatal("alice@1.1.1.1 second must fail (limit=1)")
	}
}

func TestLimiter_DifferentUsers(t *testing.T) {
	l := New(1)

	if !l.Acquire("alice", "1.1.1.1") {
		t.Fatal("alice must succeed")
	}
	if !l.Acquire("bob", "1.1.1.1") {
		t.Fatal("bob on same IP is a different key, must succeed")
	}
}

func TestLimiter_Unlimited(t *testing.T) {
	l := New(0)
	for i := 0; i < 1000; i++ {
		if !l.Acquire("alice", "1.2.3.4") {
			t.Fatalf("unlimited limiter must always allow, failed at %d", i)
		}
	}
}

func TestLimiter_Count(t *testing.T) {
	l := New(5)
	l.Acquire("alice", "1.2.3.4") //nolint:errcheck
	l.Acquire("alice", "1.2.3.4") //nolint:errcheck
	if got := l.Count("alice", "1.2.3.4"); got != 2 {
		t.Fatalf("Count: expected 2, got %d", got)
	}
	l.Release("alice", "1.2.3.4")
	if got := l.Count("alice", "1.2.3.4"); got != 1 {
		t.Fatalf("Count after release: expected 1, got %d", got)
	}
}

func TestLimiter_ReleaseNoUnderflow(t *testing.T) {
	l := New(3)
	// Release without Acquire must not panic or go negative.
	l.Release("ghost", "9.9.9.9")
	if got := l.Count("ghost", "9.9.9.9"); got != 0 {
		t.Fatalf("expected 0 after spurious release, got %d", got)
	}
}

func TestLimiter_MapCleanup(t *testing.T) {
	l := New(3)
	l.Acquire("alice", "1.2.3.4") //nolint:errcheck
	l.Release("alice", "1.2.3.4")
	l.mu.Lock()
	_, exists := l.count["alice\x001.2.3.4"]
	l.mu.Unlock()
	if exists {
		t.Fatal("map entry should be deleted when count reaches 0")
	}
}

func TestLimiter_Concurrent(t *testing.T) {
	const limit = 5
	l := New(limit)
	var wg sync.WaitGroup
	allowed := make(chan bool, 100)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed <- l.Acquire("user", "1.1.1.1")
		}()
	}
	wg.Wait()
	close(allowed)

	count := 0
	for ok := range allowed {
		if ok {
			count++
		}
	}
	if count != limit {
		t.Fatalf("concurrent: expected exactly %d acquires to succeed, got %d", limit, count)
	}
}
