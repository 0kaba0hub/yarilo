package imap

import (
	"reflect"
	"sort"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/0kaba0hub/yarilo/pkg/locks"
)

func TestEventMask(t *testing.T) {
	cases := []struct {
		name   string
		events []imaplib.NotifyEvent
		want   uint8
	}{
		{"none", nil, 0},
		{"new", []imaplib.NotifyEvent{imaplib.NotifyEventMessageNew}, notifyMaskNew},
		{"expunge", []imaplib.NotifyEvent{imaplib.NotifyEventMessageExpunge}, notifyMaskExpunge},
		{"flag", []imaplib.NotifyEvent{imaplib.NotifyEventFlagChange}, notifyMaskFlag},
		{
			"all message events",
			[]imaplib.NotifyEvent{imaplib.NotifyEventMessageNew, imaplib.NotifyEventMessageExpunge, imaplib.NotifyEventFlagChange},
			notifyMaskNew | notifyMaskExpunge | notifyMaskFlag,
		},
		{
			"mailbox-level ignored",
			[]imaplib.NotifyEvent{imaplib.NotifyEventMailboxName, imaplib.NotifyEventSubscriptionChange},
			0,
		},
		{
			"mixed keeps only message events",
			[]imaplib.NotifyEvent{imaplib.NotifyEventMailboxName, imaplib.NotifyEventMessageNew},
			notifyMaskNew,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventMask(tc.events); got != tc.want {
				t.Errorf("eventMask(%v) = %d, want %d", tc.events, got, tc.want)
			}
		})
	}
}

func TestDescendants(t *testing.T) {
	all := []string{"INBOX", "INBOX/Work", "INBOX/Work/2026", "Archive", "Archive/Old", "INBOXES"}
	cases := []struct {
		base string
		want []string
	}{
		{"INBOX", []string{"INBOX/Work", "INBOX/Work/2026"}},
		{"Archive", []string{"Archive/Old"}},
		{"INBOX/Work", []string{"INBOX/Work/2026"}},
		{"None", nil},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			got := descendants(tc.base, all)
			sort.Strings(got)
			sort.Strings(tc.want)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("descendants(%q) = %v, want %v", tc.base, got, tc.want)
			}
		})
	}
}

// TestNotifyWatcherMarkAndTake verifies the mask gating and dirty-set draining
// of the watcher without any live subscription: only events whose type is in a
// folder's mask flag it dirty, and take() clears the set.
func TestNotifyWatcherMarkAndTake(t *testing.T) {
	w := &notifyWatcher{
		dirty: make(map[string]struct{}),
		watch: map[string]uint8{
			"Work":    notifyMaskNew,
			"Archive": notifyMaskExpunge,
		},
		wake: make(chan struct{}, 1),
	}

	// Non-matching event type: Work watches New only, an Expunged event is ignored.
	w.mark("Work", locks.EventExpunged)
	// Unwatched folder is ignored.
	w.mark("Other", locks.EventDelivered)
	if got := w.take(); got != nil {
		t.Fatalf("no matching events yet, take() = %v", got)
	}

	w.mark("Work", locks.EventDelivered)    // matches
	w.mark("Archive", locks.EventExpunged)  // matches
	w.mark("Archive", locks.EventDelivered) // Archive does not watch New — ignored
	got := w.take()
	sort.Strings(got)
	want := []string{"Archive", "Work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("take() = %v, want %v", got, want)
	}
	if again := w.take(); again != nil {
		t.Fatalf("take() should be empty after drain, got %v", again)
	}
}
