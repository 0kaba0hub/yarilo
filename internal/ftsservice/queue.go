package ftsservice

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/0kaba0hub/yarilo/pkg/fts"
)

// jobSeq generates a per-process monotonic ID for every index job so its
// full lifecycle (queued -> lock wait -> checkpoint read -> index run ->
// checkpoint write, possibly spanning several log lines and goroutines) can
// be grepped as one thread instead of correlated by hand via user+folder+time.
var jobSeq atomic.Uint64

func nextJobID() uint64 { return jobSeq.Add(1) }

type job struct {
	id        uint64
	user      string
	mbox      fts.MailboxRef
	maxUID    uint32
	maxRecent int
}

// queue is a FIFO with a priority front-insert (PREPEND — on-demand search
// catch-up jumps ahead of background autoindex jobs). Duplicate jobs are
// cheap: the worker re-reads the checkpoint and no-ops when nothing is new.
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
