package warden

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestStateBackendPubSubContract runs the kick-bus contract against both
// backends (#908 PR3): a payload emitted on a channel reaches a live subscriber
// on that channel, an unrelated channel does not, and cancelling the subscriber
// context closes its channel. Memory and Redis must behave the same.
func TestStateBackendPubSubContract(t *testing.T) {
	backends := map[string]func(t *testing.T) StateBackend{
		"memory": func(*testing.T) StateBackend { return newMemoryBackend(time.Minute, time.Minute, 0) },
		"redis": func(t *testing.T) StateBackend {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { rdb.Close() })
			return NewRedisBackend(rdb, "test:warden:", "test:warden:events:", time.Minute, time.Minute, 0)
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			defer b.Close()

			ctx, cancel := context.WithCancel(context.Background())
			ch, err := b.Subscribe(ctx, "kick:imap")
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			// A subscriber on an unrelated channel must not receive our event.
			otherCtx, otherCancel := context.WithCancel(context.Background())
			defer otherCancel()
			other, err := b.Subscribe(otherCtx, "kick:pop3")
			if err != nil {
				t.Fatalf("subscribe other: %v", err)
			}

			// Redis PUBLISH only reaches subscribers already registered; the
			// SUBSCRIBE round trip above is confirmed before Subscribe returns,
			// so a short settle is enough for the relay goroutine to be reading.
			time.Sleep(50 * time.Millisecond)
			if err := b.Emit("kick:imap", "sess-1"); err != nil {
				t.Fatalf("emit: %v", err)
			}

			select {
			case got, ok := <-ch:
				if !ok {
					t.Fatal("subscriber channel closed before delivery")
				}
				if got != "sess-1" {
					t.Fatalf("payload = %q, want sess-1", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for emitted event")
			}

			select {
			case got := <-other:
				t.Fatalf("unrelated channel received %q, want nothing", got)
			case <-time.After(100 * time.Millisecond):
			}

			// Cancelling the subscriber context closes its channel.
			cancel()
			select {
			case _, ok := <-ch:
				if ok {
					// Drain a possibly-buffered value, then expect close.
					select {
					case _, ok2 := <-ch:
						if ok2 {
							t.Fatal("channel not closed after ctx cancel")
						}
					case <-time.After(time.Second):
						t.Fatal("channel not closed after ctx cancel")
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for channel close after ctx cancel")
			}
		})
	}
}
