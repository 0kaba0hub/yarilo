package ftsservice

import (
	"context"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

func TestOptimizeQueueDedupsWhilePending(t *testing.T) {
	q := newOptimizeQueue()
	user := fts.UserRef{Username: "u1"}
	mbox := fts.MailboxRef{GUID: "g1", Name: "INBOX"}

	q.push(user, mbox)
	q.push(user, mbox) // must be dropped: already queued
	q.push(user, mbox) // still dropped

	j, ok := q.pop(context.Background())
	if !ok || j.user.Username != "u1" || j.mbox.GUID != "g1" {
		t.Fatalf("pop = %+v/%v, want u1/g1", j, ok)
	}

	// The queue must now be empty — the duplicates were never enqueued.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := q.pop(ctx); ok {
		t.Fatal("queue should be empty after popping the single deduped entry")
	}
}

func TestOptimizeQueueDoneReleasesDedupMarkerForRequeue(t *testing.T) {
	q := newOptimizeQueue()
	user := fts.UserRef{Username: "u1"}
	mbox := fts.MailboxRef{GUID: "g1", Name: "INBOX"}

	q.push(user, mbox)
	j, ok := q.pop(context.Background())
	if !ok {
		t.Fatal("expected a job")
	}

	// While the job is "in flight" (popped, not yet done), a fresh rotation
	// pushing the same mailbox must still be dropped — it's covered by the
	// run already underway.
	q.push(user, mbox)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := q.pop(ctx); ok {
		t.Fatal("push while in flight (before done()) must still dedup")
	}

	// Once the run completes, a NEW rotation must be able to queue a fresh
	// pass — the whole point of clearing the marker at done(), not at pop().
	q.done(j.user, j.mbox)
	q.push(user, mbox)
	j2, ok := q.pop(context.Background())
	if !ok || j2.mbox.GUID != "g1" {
		t.Fatalf("expected a fresh job to be queued after done(): got %+v/%v", j2, ok)
	}
}

func TestOptimizeQueueDistinctMailboxesNotDeduped(t *testing.T) {
	q := newOptimizeQueue()
	user := fts.UserRef{Username: "u1"}
	a := fts.MailboxRef{GUID: "ga", Name: "INBOX"}
	b := fts.MailboxRef{GUID: "gb", Name: "Archive"}

	q.push(user, a)
	q.push(user, b)

	seen := map[string]bool{}
	for range 2 {
		j, ok := q.pop(context.Background())
		if !ok {
			t.Fatal("expected two distinct jobs")
		}
		seen[j.mbox.GUID] = true
	}
	if !seen["ga"] || !seen["gb"] {
		t.Fatalf("expected both mailboxes queued, got %v", seen)
	}
}

func TestOptimizeQueueCloseUnblocks(t *testing.T) {
	q := newOptimizeQueue()
	done := make(chan bool)
	go func() {
		_, ok := q.pop(context.Background())
		done <- ok
	}()
	q.close()
	if ok := <-done; ok {
		t.Fatal("pop on closed queue must report !ok")
	}
	// push after close is a no-op
	q.push(fts.UserRef{Username: "x"}, fts.MailboxRef{GUID: "g"})
	if _, ok := q.pop(context.Background()); ok {
		t.Fatal("closed queue must stay empty")
	}
}
