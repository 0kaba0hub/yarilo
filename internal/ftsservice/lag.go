package ftsservice

import (
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// lagTracker holds, per mailbox, how far behind its index is. It exists because
// the two numbers a user actually feels — "search does not find recent mail" —
// cannot be labels on a metric: 150 users times their folders would put that
// cardinality into Prometheus. So the per-mailbox detail stays in process and
// only the worst case is published.
//
// Entries are written at the end of a pass, when the numbers are already in
// hand, so tracking costs no extra reads.
type lagTracker struct {
	mu  sync.Mutex
	lag map[jobKey]lagSample
}

type lagSample struct {
	// uids is highest UID minus the checkpoint: how much mail is not indexed.
	uids uint32
	// oldest is the internal date of the oldest unindexed message, zero when
	// the mailbox is fully indexed.
	oldest time.Time
}

func newLagTracker() *lagTracker {
	return &lagTracker{lag: make(map[jobKey]lagSample)}
}

// observe records a mailbox's lag after a pass. msgs is the message list the
// pass already read; indexed is the checkpoint it left behind.
func (t *lagTracker) observe(key jobKey, msgs []*mailbox.MessageMeta, indexed uint32) {
	var sample lagSample
	for _, m := range msgs {
		if m.UID <= indexed {
			continue
		}
		sample.uids++
		if sample.oldest.IsZero() || m.InternalDate.Before(sample.oldest) {
			sample.oldest = m.InternalDate
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if sample.uids == 0 {
		// Fully indexed mailboxes are dropped rather than kept at zero: the
		// published number is a maximum, and a map of zeros only costs memory.
		delete(t.lag, key)
		return
	}
	t.lag[key] = sample
}

// worst returns the largest lag currently tracked. The age is computed at read
// time, so a mailbox nobody has touched keeps ageing rather than reporting the
// gap it had when its pass ran.
func (t *lagTracker) worst(now time.Time) (uids uint32, age time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.lag {
		if s.uids > uids {
			uids = s.uids
		}
		if !s.oldest.IsZero() {
			if d := now.Sub(s.oldest); d > age {
				age = d
			}
		}
	}
	return uids, age
}

// publish sets the gauges. Called on a timer rather than per pass: the value is
// a property of the whole service, and a mailbox that is behind stays behind
// between passes.
func (t *lagTracker) publish(now time.Time) {
	uids, age := t.worst(now)
	metricLagUIDs.Set(float64(uids))
	metricLagSeconds.Set(age.Seconds())
}

// lagSampleInterval is how often the gauges are refreshed. The numbers move on
// the scale of delivery, not of individual passes, so sampling often would cost
// scans without telling anyone anything new.
const lagSampleInterval = 30 * time.Second

// lagSampler publishes the gauges until the service stops.
func (s *Service) lagSampler(stop <-chan struct{}, every time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			s.lag.publish(now)
		}
	}
}
