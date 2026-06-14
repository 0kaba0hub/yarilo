// Package retry provides a simple exponential-backoff retry helper for
// startup-time dials that must succeed before a service can accept traffic.
package retry

import (
	"context"
	"log/slog"
	"time"
)

const maxDelay = 30 * time.Second

// Do calls fn up to attempts times. After each failure (except the last) it
// waits base * 2^(attempt-1), capped at 30 s, then retries. It logs each
// retry at WARN and the final failure at ERROR.
// Returns nil on the first success, or the last error after all attempts.
// Returns ctx.Err() immediately if the context is cancelled between attempts.
func Do(ctx context.Context, attempts int, base time.Duration, fn func() error) error {
	if attempts <= 0 {
		attempts = 1
	}
	var err error
	for i := range attempts {
		if i > 0 {
			delay := base * (1 << (i - 1))
			if delay > maxDelay {
				delay = maxDelay
			}
			slog.Warn("retry: attempt failed, retrying",
				"attempt", i, "of", attempts-1,
				"delay", delay,
				"err", err,
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		err = fn()
		if err == nil {
			return nil
		}
	}
	return err
}
