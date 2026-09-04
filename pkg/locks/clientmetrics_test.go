package locks

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// busyThenFree refuses the first n attempts as ErrBusy, then grants.
type busyThenFree struct{ refusals int }

func (b *busyThenFree) Lock(context.Context, string, string, time.Duration) (Lock, error) {
	if b.refusals > 0 {
		b.refusals--
		return Lock{}, ErrBusy
	}
	return Lock{ID: "granted"}, nil
}
func (b *busyThenFree) LockShared(ctx context.Context, r, o string, ttl time.Duration) (Lock, error) {
	return b.Lock(ctx, r, o, ttl)
}
func (b *busyThenFree) Unlock(context.Context, string) error               { return nil }
func (b *busyThenFree) Renew(context.Context, string, time.Duration) error { return nil }
func (b *busyThenFree) Subscribe(context.Context, string) (<-chan Event, error) {
	return nil, nil
}
func (b *busyThenFree) Emit(context.Context, string, EventType, string) error { return nil }
func (b *busyThenFree) HoldsResource(string) bool                             { return false }
func (b *busyThenFree) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}
func (b *busyThenFree) Close() error { return nil }

func counterValue(t *testing.T, resource string) float64 {
	t.Helper()
	var m dto.Metric
	c, err := clientBusyRetries.GetMetricWithLabelValues(resourceClass(resource))
	if err != nil {
		t.Fatalf("counter for %q: %v", resource, err)
	}
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// Contention is counted where it is paid.
//
// The lock service counts refusals for the whole deployment, which says how
// often somebody was refused and nothing about who waited or for what. This
// counter sits in the caller, next to the acquisition latency the sleeping
// inflates (#1533).
//
// Two refusals, one grant: three calls to the service, two sleeps. Counting the
// grant as well would make the counter a call count, which the acquisition
// histogram already is.
func TestBusyRetriesAreCountedInTheCaller(t *testing.T) {
	const resource = "mbox:alice@example.com:INBOX"
	before := counterValue(t, resource)

	l := &busyThenFree{refusals: 2}
	if _, err := Acquire(context.Background(), l, resource, "test.bin/1/alice@example.com/sess1", time.Second); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if got := counterValue(t, resource) - before; got != 2 {
		t.Errorf("counted %v busy retries for two refusals and one grant, want 2", got)
	}
}

// An uncontended acquisition counts nothing, so the counter cannot be mistaken
// for the number of acquisitions -- which is the confusion this whole issue was.
func TestAnUncontendedAcquisitionCountsNoRetry(t *testing.T) {
	const resource = "idx:alice@example.com"
	before := counterValue(t, resource)

	if _, err := Acquire(context.Background(), &busyThenFree{}, resource, "test.bin/1/alice@example.com/sess1", time.Second); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if got := counterValue(t, resource) - before; got != 0 {
		t.Errorf("counted %v retries for an acquisition nobody contended", got)
	}
}

// The label is a class, not the key.
//
// A lock key carries a username and often a folder name. Putting one into a
// metric would leak an address into monitoring and make the series unbounded --
// one label value per mailbox. So the classifier answers from a closed set, and
// anything it does not recognise is "other".
func TestTheResourceLabelIsAClosedSet(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{
		{IndexKey("alice@example.com"), "idx"},
		{MdboxMapKey("alice@example.com"), "mdboxmap"},
		{MailboxKey("alice@example.com", "INBOX"), "mbox"},
		{FTSKey("alice@example.com", "INBOX"), "fts"},
		{MailboxListKey("alice@example.com"), "mlist"},
		{DeliverKey("alice@example.com", "INBOX"), "deliver"},
		{SieveScriptsKey("alice@example.com"), "sieve"},
		{ThreadsKey("alice@example.com"), "threads"},
		{SubscriptionsKey("alice@example.com"), "subs"},
		{ACLListKey("/home/alice"), "acllist"},

		// Not from the constructors, and each one is a way the label could
		// have become a user identifier.
		{"alice@example.com", "other"},
		{"", "other"},
		{"unknownprefix:alice@example.com", "other"},
		{":alice@example.com", "other"},

		// A folder name may contain a colon, and only the first one delimits.
		{MailboxKey("alice@example.com", "Notes:2026"), "mbox"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if got := resourceClass(tc.key); got != tc.want {
				t.Errorf("resourceClass(%q) = %q, want %q", tc.key, got, tc.want)
			}
			if got := resourceClass(tc.key); strings.Contains(got, "@") || strings.Contains(got, "alice") {
				t.Errorf("the label carries part of the key: %q", got)
			}
		})
	}
}

// Every constructor in resources.go must have a class. A new lock key whose
// prefix nobody added here would land in "other", and the retries on it would
// be invisible among everything else unrecognised.
func TestEveryConstructorHasAClass(t *testing.T) {
	for _, key := range []string{
		IndexKey("u"), MdboxMapKey("u"), MailboxKey("u", "f"), FTSKey("u", "f"),
		MailboxListKey("u"), DeliverKey("u", "f"), SieveScriptsKey("u"),
		ThreadsKey("u"), SubscriptionsKey("u"), ACLListKey("/h"),
	} {
		if resourceClass(key) == "other" {
			t.Errorf("%q has no class, so its contention would be invisible", key)
		}
	}
}
