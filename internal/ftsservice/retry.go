package ftsservice

import (
	"errors"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// An index pass that cannot take the mailbox lock has not failed — it has been
// beaten to the lock by a session write, which is the normal state of a busy
// mailbox. Dropping it there means the messages it would have indexed stay
// unindexed until some unrelated event happens to queue that mailbox again,
// and a search then answers from an index that believes itself current.
//
// The pass is requeued rather than the lock waited on, because the worker count
// is one by default: waiting would park the only worker on one contended
// mailbox and stall indexing for every other user. Requeueing keeps the worker
// moving and lets the contended mailbox come back when the writer is done.
const (
	// maxIndexAttempts bounds the retry so a permanently contended mailbox
	// cannot occupy the queue forever.
	maxIndexAttempts = 6
	// retryBaseDelay is doubled per attempt: 0.5s, 1s, 2s … so a brief write
	// burst is ridden out quickly while sustained contention backs off.
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
)

// retryDelay is the wait before attempt n runs (n counted from zero).
func retryDelay(attempt int) time.Duration {
	d := retryBaseDelay << attempt //nolint:gosec // attempt is bounded by maxIndexAttempts
	if d > retryMaxDelay || d <= 0 {
		return retryMaxDelay
	}
	return d
}

// deferJob requeues j when err is transient lock contention and the attempt
// budget allows. It reports whether the job was rescheduled, so the caller can
// tell a deferred pass from a real failure.
//
// The requeue is scheduled rather than performed inline: pushing immediately
// would spin the worker against a lock that is still held.
func (s *Service) deferJob(j job, err error) bool {
	if !errors.Is(err, locks.ErrBusy) {
		return false
	}
	if j.attempt+1 >= maxIndexAttempts {
		// Out of budget: this is now a real dropped pass, and it is counted as
		// one so "the index is behind" is measurable rather than inferred from
		// log volume.
		metricIndexDropped.Inc()
		slog.Warn("fts: index job dropped after repeated lock contention",
			"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "attempts", j.attempt+1)
		return false
	}
	next := j
	next.attempt = j.attempt + 1
	delay := retryDelay(j.attempt)
	metricIndexDeferred.Inc()
	slog.Debug("fts: index job deferred, mailbox is locked",
		"job_id", j.id, "user", j.user, "folder", j.mbox.Name,
		"attempt", next.attempt, "retry_in", delay)
	// A push into a closed queue is a no-op, so a timer that fires after
	// shutdown costs nothing.
	time.AfterFunc(delay, func() { s.queue.push(next, false) })
	return true
}
