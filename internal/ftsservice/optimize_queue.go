package ftsservice

import (
	"context"
	"sync"

	"github.com/0kaba0hub/yarilo/pkg/fts"
)

// optimizeJob is one (user, mailbox) pair queued for background
// auto-optimization (#715).
type optimizeJob struct {
	user fts.UserRef
	mbox fts.MailboxRef
}

func optimizeKey(user fts.UserRef, mbox fts.MailboxRef) string {
	return user.Username + "\x00" + mbox.GUID
}

// optimizeQueue is a FIFO with true dedup, not just "cheap to re-run": a
// mailbox already queued (or currently being optimized) is never queued a
// second time — an engine can call the OptimizeNotifier callback on every
// rotation while a mailbox stays at/above its shard threshold, and that
// must not pile up duplicate work. The dedup marker is cleared only once
// the run actually finishes (see done), not when the job is popped for
// processing — a rotation that happens WHILE a compaction is already in
// flight must still be able to queue a fresh pass afterward, since it
// wasn't covered by the run already underway.
type optimizeQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	jobs    []optimizeJob
	pending map[string]bool
	closed  bool
}

func newOptimizeQueue() *optimizeQueue {
	q := &optimizeQueue{pending: map[string]bool{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push enqueues (user, mbox) unless it's already queued or in flight. It
// must stay fast and non-blocking: this is called directly from the
// engine's OptimizeNotifier callback, synchronously inside the write path.
func (q *optimizeQueue) push(user fts.UserRef, mbox fts.MailboxRef) {
	key := optimizeKey(user, mbox)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.pending[key] {
		return
	}
	q.pending[key] = true
	q.jobs = append(q.jobs, optimizeJob{user: user, mbox: mbox})
	q.cond.Signal()
}

// pop blocks until a job is available or ctx/queue is done.
func (q *optimizeQueue) pop(ctx context.Context) (optimizeJob, bool) {
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.jobs) == 0 && !q.closed && ctx.Err() == nil {
		q.cond.Wait()
	}
	if len(q.jobs) == 0 {
		return optimizeJob{}, false
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	return j, true
}

// done clears the dedup marker after a run completes (successfully or not),
// letting a rotation that occurred mid-run queue a fresh pass.
func (q *optimizeQueue) done(user fts.UserRef, mbox fts.MailboxRef) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, optimizeKey(user, mbox))
}

func (q *optimizeQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}
