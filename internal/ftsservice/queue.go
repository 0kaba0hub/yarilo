package ftsservice

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// jobSeq: per-process monotonic job ID, so one job's log lines can be
// grepped as a single thread.
var jobSeq atomic.Uint64

func nextJobID() uint64 { return jobSeq.Add(1) }

type job struct {
	id        uint64
	user      string
	mbox      fts.MailboxRef
	maxUID    uint32
	maxRecent int
	// attempt counts how many times this pass has been deferred for transient
	// lock contention. It bounds the retry: a mailbox that is busy forever must
	// not be retried forever.
	attempt int
}

// queue is a FIFO with a priority front-insert (PREPEND: search catch-up
// jumps ahead of autoindex). Duplicates are cheap: the worker re-reads
// the checkpoint and no-ops.
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	jobs   []job
	closed bool
}

func newQueue() *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) push(j job, front bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if front {
		q.jobs = append([]job{j}, q.jobs...)
	} else {
		q.jobs = append(q.jobs, j)
	}
	metricQueueDepth.Set(float64(len(q.jobs)))
	q.cond.Signal()
}

// pop blocks until a job is available or ctx/queue is done.
func (q *queue) pop(ctx context.Context) (job, bool) {
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
		return job{}, false
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	return j, true
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// depth returns the number of pending jobs.
func (q *queue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}
