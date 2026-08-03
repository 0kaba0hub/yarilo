package ftsservice

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"

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

// jobKey identifies the work, which is a mailbox — not a request. Several
// requests for one mailbox are one pass over it.
type jobKey struct {
	user   string
	folder string
}

func (j job) key() jobKey { return jobKey{user: j.user, folder: j.mbox.Name} }

// entry is one mailbox's place in the queue.
type entry struct {
	job job
	// el is the position in the order list; nil while the pass is running.
	el *list.Element
	// queuedAt measures how long work waited, which is the queue's own health.
	queuedAt time.Time
	// running is true between pop and done.
	running bool
	// requeue records a request that arrived while the pass was running. The
	// running pass read its checkpoint before that request existed, so it will
	// not cover it: the mailbox goes back into the queue instead of leaving.
	requeue bool
	// front carries the priority of the request that set requeue.
	front bool
}

// queue holds at most one pending pass per mailbox.
//
// Coalescing is a correctness property, not an optimisation. A pass reads the
// checkpoint and indexes everything above it, so two passes over one mailbox do
// the same work twice — and the second discovers this only after taking the
// lock. Under delivery load those duplicates are what turn a busy mailbox into
// a stream of lock contention.
//
// A request that arrives while its mailbox is being indexed cannot be dropped
// either: the running pass has already read its checkpoint and will not see the
// new messages. It is recorded on the running entry and re-queued when the pass
// finishes, so nothing is lost and nothing is duplicated.
//
// Ordering is FIFO with a priority front-insert — a search catch-up jumps ahead
// of background autoindex. A front-insert for a mailbox already queued moves it
// rather than adding a second entry.
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	order  *list.List // of *entry, highest priority first
	index  map[jobKey]*entry
	closed bool
}

func newQueue() *queue {
	q := &queue{order: list.New(), index: make(map[jobKey]*entry)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push queues a pass over j's mailbox, merging into one already queued.
func (q *queue) push(j job, front bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	key := j.key()
	if e, ok := q.index[key]; ok {
		e.merge(j)
		metricQueueMerged.Inc()
		if e.running {
			e.requeue = true
			e.front = e.front || front
			return
		}
		if front && e.el != nil {
			q.order.MoveToFront(e.el)
		}
		q.wake()
		return
	}
	e := &entry{job: j, queuedAt: time.Now()}
	if front {
		e.el = q.order.PushFront(e)
	} else {
		e.el = q.order.PushBack(e)
	}
	q.index[key] = e
	q.wake()
}

// merge folds a new request into the pending one. The pass must cover the
// highest UID either request asked for, or the later request's messages would
// be skipped. max_recent takes the smaller bound: it is a limit, and the
// stricter of two limits is the one both callers can live with.
func (e *entry) merge(j job) {
	if j.maxUID > e.job.maxUID {
		e.job.maxUID = j.maxUID
	}
	switch {
	case e.job.maxRecent == 0:
		e.job.maxRecent = j.maxRecent
	case j.maxRecent > 0 && j.maxRecent < e.job.maxRecent:
		e.job.maxRecent = j.maxRecent
	}
	// A fresh request restarts the backoff: it is new evidence that the mailbox
	// wants indexing, not a continuation of the attempt that failed.
	if j.attempt < e.job.attempt {
		e.job.attempt = j.attempt
	}
}

func (q *queue) wake() {
	metricQueueDepth.Set(float64(q.order.Len()))
	q.cond.Signal()
}

// pop blocks until a job is available or ctx/queue is done. The mailbox stays
// in the index while it runs, so a request arriving meanwhile coalesces onto it
// rather than becoming a second pass.
//
// Every popped job must be handed back through done, or that mailbox is never
// queued again — the caller defers it immediately after popping.
func (q *queue) pop(ctx context.Context) (job, bool) {
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
	defer stop()

	q.mu.Lock()
	defer q.mu.Unlock()
	for q.order.Len() == 0 && !q.closed && ctx.Err() == nil {
		q.cond.Wait()
	}
	el := q.order.Front()
	if el == nil {
		return job{}, false
	}
	q.order.Remove(el)
	e, _ := el.Value.(*entry)
	e.el = nil
	e.running = true
	metricQueueWait.Observe(time.Since(e.queuedAt).Seconds())
	metricQueueDepth.Set(float64(q.order.Len()))
	return e.job, true
}

// done releases a mailbox after its pass. A request that arrived while the pass
// was running puts the mailbox straight back into the queue; otherwise it
// leaves, and the next request for it starts a fresh entry.
func (q *queue) done(j job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := j.key()
	e, ok := q.index[key]
	if !ok {
		return
	}
	e.running = false
	if !e.requeue || q.closed {
		delete(q.index, key)
		metricQueueDepth.Set(float64(q.order.Len()))
		return
	}
	e.requeue = false
	e.queuedAt = time.Now()
	metricQueueRequeued.Inc()
	if e.front {
		e.front = false
		e.el = q.order.PushFront(e)
	} else {
		e.el = q.order.PushBack(e)
	}
	q.wake()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// depth returns the number of pending passes. A running pass is not pending.
func (q *queue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.order.Len()
}
