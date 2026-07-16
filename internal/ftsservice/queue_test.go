package ftsservice

import (
	"context"
	"testing"

	"github.com/0kaba0hub/yarilo/pkg/fts"
)

func TestQueueOrderAndPrepend(t *testing.T) {
	q := newQueue()
	mb := fts.MailboxRef{Name: "INBOX"}
	q.push(job{user: "a", mbox: mb}, false)
	q.push(job{user: "b", mbox: mb}, false)
	q.push(job{user: "urgent", mbox: mb}, true) // PREPEND jumps the line
	q.push(job{user: "c", mbox: mb}, false)

	want := []string{"urgent", "a", "b", "c"}
	for _, w := range want {
		j, ok := q.pop(context.Background())
		if !ok || j.user != w {
			t.Fatalf("pop = %q/%v, want %q", j.user, ok, w)
		}
	}
}

func TestQueueCloseUnblocks(t *testing.T) {
	q := newQueue()
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
	q.push(job{user: "x"}, false)
	if _, ok := q.pop(context.Background()); ok {
		t.Fatal("closed queue must stay empty")
	}
}

func TestQueueContextCancel(t *testing.T) {
	q := newQueue()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		_, ok := q.pop(ctx)
		done <- ok
	}()
	cancel()
	if ok := <-done; ok {
		t.Fatal("pop must unblock on context cancel")
	}
}
