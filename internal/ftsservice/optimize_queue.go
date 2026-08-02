package ftsservice

import (
	"context"
	"sync"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// optimizeJob is one (user, mailbox) pair queued for background
// auto-optimization.
type optimizeJob struct {
	user fts.UserRef
	mbox fts.MailboxRef
}

func optimizeKey(user fts.UserRef, mbox fts.MailboxRef) string {
	return user.Username + "\x00" + mbox.GUID
}

// optimizeQueue is a FIFO with dedup: a mailbox already queued or in
// flight is never queued twice. The dedup marker is cleared at done(),
// not pop() — a rotation during a running compaction must still be able
// to queue a fresh pass afterward.
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

// push enqueues (user, mbox) unless already queued or in flight. Must
// stay non-blocking: called synchronously from the engine's write path.
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

// done clears the dedup marker after a run completes (success or not).
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
