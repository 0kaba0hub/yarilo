package ftsservice

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Utilisation has to survive the sampling interval. A pass takes tens of
// milliseconds and Prometheus scrapes every 15-30 seconds, so a gauge of
// "workers busy now" reads zero whether the service is saturated or idle — the
// metric looked like an answer to a question it could not reach.
func TestWorkerBusyTimeAccumulates(t *testing.T) {
	s := &Service{
		queue: newQueue(),
		lag:   newLagTracker(),
		opts: Options{
			ResolveUser: func(string) (*mailbox.UserInfo, error) {
				// Failing fast: the counter records the time a worker was
				// occupied, not the time it was productive. A worker stuck on
				// failing passes has no spare capacity either.
				return nil, errors.New("userdb down")
			},
		},
	}

	before := testutil.ToFloat64(metricWorkerBusySeconds)
	for i := 0; i < 5; i++ {
		s.queue.push(inbox("u1"), false)
		j, ok := s.queue.pop(context.Background())
		if !ok {
			t.Fatal("nothing queued")
		}
		s.runPass(j)
	}
	if after := testutil.ToFloat64(metricWorkerBusySeconds); after <= before {
		t.Errorf("busy time did not move over five passes: %v -> %v — utilisation cannot be derived",
			before, after)
	}
}

func inbox(user string) job {
	return job{user: user, mbox: fts.MailboxRef{Name: "INBOX"}}
}

// A mailbox is the unit of work: several requests for one mailbox are one pass.
// Two passes would do the same work twice, and the second would discover this
// only after taking the lock — which is how duplicates turn into contention.
func TestQueueCoalescesRequestsForOneMailbox(t *testing.T) {
	q := newQueue()
	for i := 0; i < 5; i++ {
		q.push(inbox("u1"), false)
	}
	if got := q.depth(); got != 1 {
		t.Fatalf("depth = %d after five requests for one mailbox, want 1", got)
	}
	// Different mailboxes of one user stay distinct.
	q.push(job{user: "u1", mbox: fts.MailboxRef{Name: "Sent"}}, false)
	if got := q.depth(); got != 2 {
		t.Errorf("depth = %d, want 2 — a second mailbox is separate work", got)
	}
}

// The merged pass must cover the highest UID anyone asked for, or the later
// request's messages are skipped until something else queues the mailbox.
func TestQueueMergeKeepsTheWidestRange(t *testing.T) {
	q := newQueue()
	a := inbox("u1")
	a.maxUID, a.maxRecent = 100, 50
	b := inbox("u1")
	b.maxUID, b.maxRecent = 250, 10

	q.push(a, false)
	q.push(b, false)

	j, ok := q.pop(context.Background())
	if !ok {
		t.Fatal("nothing queued")
	}
	if j.maxUID != 250 {
		t.Errorf("maxUID = %d, want 250 — the merged pass must reach the later request", j.maxUID)
	}
	if j.maxRecent != 10 {
		t.Errorf("maxRecent = %d, want 10 — a limit merges to the stricter bound", j.maxRecent)
	}
}

// A priority request for a mailbox already queued moves it forward instead of
// adding a second entry.
func TestQueuePriorityMovesRatherThanDuplicates(t *testing.T) {
	q := newQueue()
	q.push(inbox("first"), false)
	q.push(inbox("second"), false)
	q.push(inbox("second"), true) // search catch-up for a queued mailbox

	if got := q.depth(); got != 2 {
		t.Fatalf("depth = %d, want 2", got)
	}
	j, _ := q.pop(context.Background())
	if j.user != "second" {
		t.Errorf("front = %q, want the prioritised mailbox", j.user)
	}
}

// A request arriving mid-pass cannot be dropped: the running pass read its
// checkpoint before that request existed, so it will not cover it.
func TestQueueRequeuesARequestThatArrivedMidPass(t *testing.T) {
	q := newQueue()
	q.push(inbox("u1"), false)

	j, ok := q.pop(context.Background())
	if !ok {
		t.Fatal("nothing queued")
	}
	// While the pass runs, new mail arrives for the same mailbox.
	q.push(inbox("u1"), false)
	if got := q.depth(); got != 0 {
		t.Errorf("depth = %d while the pass runs, want 0 — it must not be a second entry", got)
	}
	q.done(j)
	if got := q.depth(); got != 1 {
		t.Fatalf("depth = %d after the pass, want 1 — the mid-pass request must come back", got)
	}
	if _, ok := q.pop(context.Background()); !ok {
		t.Error("the re-queued mailbox is not poppable")
	}
}

// Without a mid-pass request the mailbox simply leaves.
func TestQueueDoneReleasesWhenNothingArrived(t *testing.T) {
	q := newQueue()
	q.push(inbox("u1"), false)
	j, _ := q.pop(context.Background())
	q.done(j)
	if got := q.depth(); got != 0 {
		t.Errorf("depth = %d, want 0", got)
	}
	// And the mailbox can be queued again afterwards.
	q.push(inbox("u1"), false)
	if got := q.depth(); got != 1 {
		t.Errorf("depth = %d, want 1 — a released mailbox must be queueable", got)
	}
}

// The failure this design can produce is a mailbox claimed forever: silently
// unindexed mail. A pass that panics must still release it, which is why the
// worker defers done rather than calling it on the success path.
func TestQueueDoneSurvivesAPanickingPass(t *testing.T) {
	q := newQueue()
	q.push(inbox("u1"), false)
	j, _ := q.pop(context.Background())

	func() {
		defer func() {
			_ = recover()
		}()
		defer q.done(j) // the worker's own discipline, exercised here
		panic("pass exploded")
	}()

	q.push(inbox("u1"), false)
	if got := q.depth(); got != 1 {
		t.Fatalf("depth = %d after a panicking pass, want 1 — the mailbox is claimed forever", got)
	}
}

// done for a mailbox that is not claimed must not resurrect it or panic.
func TestQueueDoneOnUnknownMailboxIsHarmless(t *testing.T) {
	q := newQueue()
	q.done(inbox("ghost"))
	if got := q.depth(); got != 0 {
		t.Errorf("depth = %d, want 0", got)
	}
}

// The discipline that matters is the worker's, not the queue's: runPass must
// release the mailbox however it exits. A test that only exercises the queue
// passes with the defer removed, which is exactly the mistake worth catching.
func TestRunPassReleasesTheMailboxOnPanic(t *testing.T) {
	s := &Service{
		queue: newQueue(),
		lag:   newLagTracker(),
		opts: Options{
			ResolveUser: func(string) (*mailbox.UserInfo, error) {
				panic("userdb exploded mid-pass")
			},
		},
	}
	s.queue.push(inbox("u1"), false)
	j, ok := s.queue.pop(context.Background())
	if !ok {
		t.Fatal("nothing queued")
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the pass did not panic; the test is not exercising the path it claims")
			}
		}()
		s.runPass(j)
	}()

	s.queue.push(inbox("u1"), false)
	if got := s.queue.depth(); got != 1 {
		t.Fatalf("depth = %d, want 1 — a panicking pass left the mailbox claimed forever", got)
	}
}

// The same for the ordinary failure path: an error must release it too.
func TestRunPassReleasesTheMailboxOnError(t *testing.T) {
	s := &Service{
		queue: newQueue(),
		lag:   newLagTracker(),
		opts: Options{
			ResolveUser: func(string) (*mailbox.UserInfo, error) {
				return nil, errors.New("userdb down")
			},
		},
	}
	s.queue.push(inbox("u1"), false)
	j, _ := s.queue.pop(context.Background())
	s.runPass(j)

	s.queue.push(inbox("u1"), false)
	if got := s.queue.depth(); got != 1 {
		t.Errorf("depth = %d, want 1 — a failed pass left the mailbox claimed", got)
	}
}
