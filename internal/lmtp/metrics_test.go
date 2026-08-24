package lmtp

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// histCount returns how many observations a histogram holds, summed over the
// series matching label, or over all series when label is empty.
func histCount(t *testing.T, name, label string) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out uint64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if label != "" {
				var match bool
				for _, l := range m.GetLabel() {
					match = match || l.GetValue() == label
				}
				if !match {
					continue
				}
			}
			out += m.GetHistogram().GetSampleCount()
		}
	}
	return out
}

func metricsSession(t *testing.T, threading bool) (*session, *mailbox.Resolver) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	opts := Options{Mailbox: maildir.New(), Index: fileindex.New(), Resolver: resolver}
	if threading {
		opts.Threads = threads.NewRecorder(threads.NewCache(time.Minute))
	}
	s := &session{opts: opts}
	s.from = "sender@x"
	return s, resolver
}

// A delivery is timed end to end. Without this the cost of the threading knob
// -- the thing its default flip turns on for everyone -- is visible in a
// sandbox stopwatch and nowhere else.
func TestADeliveryIsTimed(t *testing.T) {
	s, _ := metricsSession(t, true)
	s.rcpts = []string{"alice@example.com"}

	before := histCount(t, "lmtp_delivery_seconds", "delivered")
	raw := "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n"
	if err := s.LMTPData(strings.NewReader(raw), &statusSink{}); err != nil {
		t.Fatalf("LMTPData: %v", err)
	}
	if got := histCount(t, "lmtp_delivery_seconds", "delivered") - before; got != 1 {
		t.Errorf("delivered observations rose by %d, want 1", got)
	}
}

// The sidecar write has its own series, so "delivery got slower" and "delivery
// got slower because of threading" are two different readings rather than one
// guess.
func TestTheThreadingWriteIsTimedSeparately(t *testing.T) {
	s, _ := metricsSession(t, true)
	s.rcpts = []string{"alice@example.com"}

	before := histCount(t, "lmtp_thread_record_seconds", "")
	raw := "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n"
	if err := s.LMTPData(strings.NewReader(raw), &statusSink{}); err != nil {
		t.Fatalf("LMTPData: %v", err)
	}
	if got := histCount(t, "lmtp_thread_record_seconds", "") - before; got != 1 {
		t.Errorf("thread-record observations rose by %d, want 1", got)
	}
}

// With the knob off the narrow series must not move, or the A/B the QA window
// rests on compares two numbers that both include threading.
func TestTheThreadingSeriesIsSilentWhenTheKnobIsOff(t *testing.T) {
	s, _ := metricsSession(t, false)
	s.rcpts = []string{"alice@example.com"}

	beforeThread := histCount(t, "lmtp_thread_record_seconds", "")
	beforeDelivery := histCount(t, "lmtp_delivery_seconds", "delivered")
	raw := "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n"
	if err := s.LMTPData(strings.NewReader(raw), &statusSink{}); err != nil {
		t.Fatalf("LMTPData: %v", err)
	}
	if got := histCount(t, "lmtp_thread_record_seconds", "") - beforeThread; got != 0 {
		t.Errorf("thread-record observations rose by %d with threading off, want 0", got)
	}
	// The delivery itself is still timed: the A/B needs both sides measured by
	// the same instrument.
	if got := histCount(t, "lmtp_delivery_seconds", "delivered") - beforeDelivery; got != 1 {
		t.Errorf("delivered observations rose by %d, want 1", got)
	}
}

// A rejected recipient is timed too, under its own outcome. A mailbox that
// says no slowly is still slow, and the quota path that says it walks every
// folder to decide.
func TestARejectionIsTimed(t *testing.T) {
	s, _ := metricsSession(t, false)
	s.opts.QuotaEngine = true
	s.opts.QuotaMailSize = 1 // every message is too large
	s.rcpts = []string{"alice@example.com"}

	before := histCount(t, "lmtp_delivery_seconds", "rejected")
	raw := "Message-ID: <root@x>\r\nSubject: Plan\r\n\r\nbody\r\n"
	sink := &statusSink{}
	if err := s.LMTPData(strings.NewReader(raw), sink); err != nil {
		t.Fatalf("LMTPData: %v", err)
	}
	if sink.errs["alice@example.com"] == nil {
		t.Fatal("the delivery was accepted; this row needs a rejection")
	}
	if got := histCount(t, "lmtp_delivery_seconds", "rejected") - before; got != 1 {
		t.Errorf("rejected observations rose by %d, want 1", got)
	}
}
