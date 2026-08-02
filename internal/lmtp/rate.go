package lmtp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// ErrRateLimited is returned by checkRecipientRate when the
// (sender IP, recipient mailbox) pair has consumed its burst
// within the current window. Callers surface it to the SMTP
// client as `421 4.7.0`.
var ErrRateLimited = errors.New("lmtp/rate: recipient rate limit exceeded")

// checkRecipientRate enforces the per-(IP, mailbox) token bucket
// at RCPT TO. Returns nil when the delivery may proceed, or
// ErrRateLimited when the cluster-wide counter for the current
// window has crossed burst.
//
// Counter strategy:
//
//   - Key: `lmtp:rate:<ip>:<mailbox>:<bucket-id>`
//     where bucket-id = floor(now.Unix() / windowSeconds)
//   - On every RCPT, INC the counter by 1; if the resulting
//     value > burst, deny.
//   - Old bucket keys expire naturally on the next read of a
//     newer bucket-id (we never look at them again). pkg/locks
//     counters have no TTL, but stale keys consume O(1) memory
//     per (IP, mailbox, window) tuple — periodic purge on the
//     locks backend is a follow-up, not a correctness gap.
//
// A nil locker disables the check entirely (single-pod tests,
// embedded mode without a counter backend) — returns nil. An
// IncrementCounter transport error logs at the caller and is
// treated as "allow": availability of the rate-limit subsystem
// must never block legitimate delivery.
func checkRecipientRate(ctx context.Context, locker locks.Locker, ip, mailbox string, burst, windowSeconds int) error {
	if locker == nil || burst <= 0 || windowSeconds <= 0 {
		return nil
	}
	bucket := time.Now().UTC().Unix() / int64(windowSeconds)
	key := fmt.Sprintf("lmtp:rate:%s:%s:%d", ip, mailbox, bucket)
	count, err := locker.IncrementCounter(ctx, key, 1)
	if err != nil {
		return fmt.Errorf("lmtp/rate: counter inc: %w", err)
	}
	if count > int64(burst) {
		return ErrRateLimited
	}
	return nil
}
