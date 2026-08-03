package ftsservice

import (
	"context"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/fts"
)

func mbox(user, folder string) job {
	return job{user: user, mbox: fts.MailboxRef{Name: folder}}
}

// The engine holds one mutex per user, so a second pass on the same user would
// occupy a worker to wait inside it. Dispatch hands the next worker a different
// user instead — which is also what stops one user with many mailboxes taking
// every worker.
func TestDispatchSkipsAUserAlreadyRunning(t *testing.T) {
	q := newQueue()
	q.push(mbox("u1", "INBOX"), false)
	q.push(mbox("u1", "Sent"), false)
	q.push(mbox("u2", "INBOX"), false)

	first, ok := q.pop(context.Background())
	if !ok || first.user != "u1" {
		t.Fatalf("first pop = %q/%v, want u1", first.user, ok)
	}
	second, ok := q.pop(context.Background())
	if !ok {
		t.Fatal("second pop found nothing, though another user was queued")
	}
	if second.user != "u2" {
		t.Errorf("second pop = %q/%q, want u2 — u1 is already being indexed",
			second.user, second.mbox.Name)
	}
}

// Skipped work is not lost: once the user is released it is dispatched.
func TestDispatchReleasesSkippedWorkAfterDone(t *testing.T) {
	q := newQueue()
	q.push(mbox("u1", "INBOX"), false)
	q.push(mbox("u1", "Sent"), false)

	first, _ := q.pop(context.Background())

	// While u1 runs, its other mailbox is not dispatchable.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, ok := q.pop(ctx); ok {
		cancel()
		t.Fatal("dispatched a second pass for a user already running")
	}
	cancel()

	q.done(first)
	second, ok := q.pop(context.Background())
	if !ok {
		t.Fatal("the skipped mailbox was never dispatched after the user was released")
	}
	if second.user != "u1" || second.mbox.Name != "Sent" {
		t.Errorf("second pop = %q/%q, want u1/Sent", second.user, second.mbox.Name)
	}
}

// A worker parked because everything queued belonged to busy users must wake
// when one is released — otherwise it sleeps while work is available.
func TestDispatchWakesAParkedWorker(t *testing.T) {
	q := newQueue()
	q.push(mbox("u1", "INBOX"), false)
	q.push(mbox("u1", "Sent"), false)
	first, _ := q.pop(context.Background())

	got := make(chan job, 1)
	go func() {
		j, ok := q.pop(context.Background())
		if ok {
			got <- j
		}
	}()

	// Give the goroutine time to park on the condition.
	time.Sleep(50 * time.Millisecond)
	q.done(first)

	select {
	case j := <-got:
		if j.mbox.Name != "Sent" {
			t.Errorf("woke with %q, want Sent", j.mbox.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a parked worker did not wake when the user was released")
	}
}

// Priority still decides among eligible entries: a busy user's mailbox does not
// hold up a lower-priority one belonging to somebody else.
func TestDispatchHonoursPriorityAmongEligibleUsers(t *testing.T) {
	q := newQueue()
	q.push(mbox("u1", "INBOX"), false)
	first, _ := q.pop(context.Background()) // u1 now running

	q.push(mbox("u2", "INBOX"), false)
	q.push(mbox("u1", "Sent"), false) // not eligible
	q.push(mbox("u3", "INBOX"), true) // priority, eligible

	next, ok := q.pop(context.Background())
	if !ok {
		t.Fatal("nothing dispatched")
	}
	if next.user != "u3" {
		t.Errorf("dispatched %q, want u3 — priority among eligible users", next.user)
	}
	q.done(first)
}

// Closing releases workers parked on ineligible work, or shutdown hangs.
func TestDispatchCloseUnblocksAParkedWorker(t *testing.T) {
	q := newQueue()
	q.push(mbox("u1", "INBOX"), false)
	q.push(mbox("u1", "Sent"), false)
	q.pop(context.Background()) //nolint:errcheck // u1 now running

	done := make(chan bool, 1)
	go func() {
		_, ok := q.pop(context.Background())
		done <- ok
	}()
	time.Sleep(50 * time.Millisecond)
	q.close()

	select {
	case ok := <-done:
		if ok {
			t.Error("pop returned a job after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not release a worker parked on ineligible work")
	}
}
