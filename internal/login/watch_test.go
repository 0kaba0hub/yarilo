package login

import (
	"context"
	"testing"
	"time"
)

// TestWatch_NoDirectorReturns guards #736's standalone guard: Watch is a no-op
// (returns immediately) when no director is configured, rather than spinning a
// dial-fail/backoff loop.
func TestWatch_NoDirectorReturns(t *testing.T) {
	s := New(Options{}) // DirectorAddr == ""
	done := make(chan struct{})
	go func() { s.Watch(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch with no DirectorAddr should return immediately")
	}
}
