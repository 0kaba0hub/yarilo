package ftsservice

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// readTimer counts the time actually spent blocked reading message bytes.
//
// It exists because the obvious split was wrong: metricFetch times box.Fetch,
// which is the open, while the body streams into Build through this reader —
// so every byte of storage I/O was being counted as tokenisation. Without this
// there is no way to say what overlapping reads recovered, only that something
// changed.
type readTimer struct {
	r     io.Reader
	spent time.Duration
}

func (t *readTimer) Read(p []byte) (int, error) {
	t0 := time.Now()
	n, err := t.r.Read(p)
	t.spent += time.Since(t0)
	return n, err
}

// fetched is one message the pipeline produced, in UID order.
//
// err travels with the message rather than ending the run: a read failure
// belongs to a specific UID, and the consumer must attribute it to that UID
// without advancing the checkpoint past it. Losing that association is the one
// way this change could corrupt indexing progress.
type fetched struct {
	meta *mailbox.MessageMeta
	body []byte
	err  error
}

// prefetchOptions bound the window. Both limits are needed: counting messages
// alone lets four 30 MB attachments sit in memory at once, and counting bytes
// alone lets a mailbox of tiny messages open unbounded files.
type prefetchOptions struct {
	Depth    int
	MaxBytes int64
}

// enabled reports whether prefetching does anything. Depth below two means the
// next message is only read once the current one is done, which is the
// sequential path — kept as one code path rather than two.
func (o prefetchOptions) enabled() bool { return o.Depth >= 2 && o.MaxBytes > 0 }

// fetcher reads messages ahead of the consumer, in order.
type fetcher struct {
	h    *userHandle
	name string
	opts prefetchOptions

	mu       sync.Mutex
	cond     *sync.Cond
	inflight int64
}

func newFetcher(h *userHandle, folder string, opts prefetchOptions) *fetcher {
	f := &fetcher{h: h, name: folder, opts: opts}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// run reads msgs into the returned channel in order. The caller must drain it,
// or cancel ctx: the producer holds a reservation for the message it is
// reading and would otherwise block forever.
//
// Order is not an optimisation detail here. Indexing walks UIDs upwards and the
// checkpoint advances monotonically, so a pipeline that reordered would record
// progress past messages it never indexed. One producer feeding one channel
// makes reordering impossible rather than merely unlikely.
func (f *fetcher) run(ctx context.Context, msgs []*mailbox.MessageMeta) <-chan fetched {
	depth := f.opts.Depth
	if depth < 1 {
		depth = 1
	}
	out := make(chan fetched, depth-1)
	go func() {
		defer close(out)
		// The gauge is per-pass: leaving the last value behind would report a
		// window that is full when nothing is running.
		defer func() {
			f.mu.Lock()
			f.inflight = 0
			f.mu.Unlock()
			metricPrefetchInflight.Set(0)
		}()
		for _, m := range msgs {
			if ctx.Err() != nil {
				return
			}
			size := int64(m.Size)
			if !f.reserve(ctx, size) {
				return
			}
			item := f.read(m)
			select {
			case out <- item:
			case <-ctx.Done():
				f.release(size)
				return
			}
		}
	}()
	return out
}

// reserve waits until size fits in the window. A message larger than the whole
// ceiling is admitted alone rather than deadlocking: the ceiling bounds
// concurrency, and refusing to index a large message would be worse than
// briefly exceeding it.
func (f *fetcher) reserve(ctx context.Context, size int64) bool {
	stop := context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer stop()

	f.mu.Lock()
	defer f.mu.Unlock()
	t0 := time.Now()
	stalled := false
	for f.inflight > 0 && f.inflight+size > f.opts.MaxBytes {
		if ctx.Err() != nil {
			return false
		}
		stalled = true
		f.cond.Wait()
	}
	if stalled {
		// How long the reader waited for the consumer: a window that is always
		// full means the ceiling is the bottleneck, not the disk.
		metricPrefetchStall.Observe(time.Since(t0).Seconds())
	}
	f.inflight += size
	metricPrefetchInflight.Set(float64(f.inflight))
	return true
}

func (f *fetcher) release(size int64) {
	f.mu.Lock()
	f.inflight -= size
	if f.inflight < 0 {
		f.inflight = 0
	}
	metricPrefetchInflight.Set(float64(f.inflight))
	f.cond.Signal()
	f.mu.Unlock()
}

// read pulls one message into memory, timing the open and the read separately.
func (f *fetcher) read(m *mailbox.MessageMeta) fetched {
	tOpen := time.Now()
	rc, err := f.h.box.Fetch(f.name, m.Filename, m.AltTier)
	metricFetch.Observe(time.Since(tOpen).Seconds())
	if err != nil {
		return fetched{meta: m, err: err}
	}
	defer rc.Close() //nolint:errcheck

	t := &readTimer{r: rc}
	body, err := io.ReadAll(t)
	metricRead.Observe(t.spent.Seconds())
	metricMessageBytes.Observe(float64(m.Size))
	if err != nil {
		return fetched{meta: m, err: err}
	}
	return fetched{meta: m, body: body}
}
