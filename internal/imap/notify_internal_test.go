package imap

import (
	"reflect"
	"sort"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/pkg/locks"
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

func TestWantsEvent(t *testing.T) {
	items := []imaplib.NotifyItem{
		{MailboxSpec: imaplib.NotifyMailboxSpecSelected, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMailboxName}},
		{MailboxSpec: imaplib.NotifyMailboxSpecPersonal, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMessageNew, imaplib.NotifyEventMailboxName}},
	}
	if !wantsEvent(items, imaplib.NotifyEventMailboxName) {
		t.Error("MailboxName on the PERSONAL filter should be wanted")
	}
	if wantsEvent(items, imaplib.NotifyEventSubscriptionChange) {
		t.Error("SubscriptionChange was not requested")
	}
	// A mailbox-level event listed only on SELECTED must not count as a
	// non-selected request.
	selOnly := []imaplib.NotifyItem{
		{MailboxSpec: imaplib.NotifyMailboxSpecSelected, Events: []imaplib.NotifyEvent{imaplib.NotifyEventFlagChange}},
	}
	if wantsEvent(selOnly, imaplib.NotifyEventFlagChange) {
		t.Error("SELECTED-only events must not be treated as non-selected requests")
	}
}

func TestMatchStaticFilters(t *testing.T) {
	w := &notifyWatcher{items: []imaplib.NotifyItem{
		{MailboxSpec: imaplib.NotifyMailboxSpecInboxes, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMessageNew}},
		{Mailboxes: []string{"Work"}, Subtree: true, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMessageExpunge}},
		{Mailboxes: []string{"Exact"}, Events: []imaplib.NotifyEvent{imaplib.NotifyEventFlagChange}},
		// SUBSCRIBED must never match statically.
		{MailboxSpec: imaplib.NotifyMailboxSpecSubscribed, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMessageNew}},
	}}
	cases := []struct {
		folder string
		want   uint8
	}{
		{"INBOX", notifyMaskNew},
		{"INBOX/sub", notifyMaskNew},
		{"Work", notifyMaskExpunge},
		{"Work/2026", notifyMaskExpunge}, // subtree
		{"Exact", notifyMaskFlag},
		{"Exact/child", 0}, // no subtree on the exact filter
		{"Unrelated", 0},
	}
	for _, tc := range cases {
		if got := w.matchStaticFilters(tc.folder); got != tc.want {
			t.Errorf("matchStaticFilters(%q) = %d, want %d", tc.folder, got, tc.want)
		}
	}
	if m := w.subscribedMask(); m != notifyMaskNew {
		t.Errorf("subscribedMask() = %d, want %d", m, notifyMaskNew)
	}
}

func TestHandleListEvent(t *testing.T) {
	newWatcher := func(items []imaplib.NotifyItem, wantName, wantSub bool) *notifyWatcher {
		w := &notifyWatcher{
			dirty:      make(map[string]struct{}),
			watch:      make(map[string]uint8),
			subscribed: make(map[string]struct{}),
			items:      items,
			wantName:   wantName,
			wantSub:    wantSub,
			wake:       make(chan struct{}, 1),
		}
		// Stand-in for the real subscription: just arm the mask.
		w.addSub = func(folder string, mask uint8) {
			w.mu.Lock()
			w.watch[folder] |= mask
			w.mu.Unlock()
		}
		return w
	}

	personal := []imaplib.NotifyItem{
		{MailboxSpec: imaplib.NotifyMailboxSpecPersonal, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMessageNew, imaplib.NotifyEventMailboxName}},
	}

	t.Run("create matching PERSONAL adds watch and LIST", func(t *testing.T) {
		w := newWatcher(personal, true, false)
		w.handleListEvent(locks.Event{Type: locks.EventMailboxCreate, Payload: "New"})
		if w.watch["New"] != notifyMaskNew {
			t.Errorf("watch[New] = %d, want %d", w.watch["New"], notifyMaskNew)
		}
		lists := w.takeLists()
		if len(lists) != 1 || lists[0].Mailbox != "New" || len(lists[0].Attrs) != 0 {
			t.Errorf("create LIST = %+v", lists)
		}
	})

	t.Run("delete removes watch and emits NonExistent", func(t *testing.T) {
		w := newWatcher(personal, true, false)
		w.addSub("Gone", notifyMaskNew)
		w.handleListEvent(locks.Event{Type: locks.EventMailboxDelete, Payload: "Gone"})
		if _, ok := w.watch["Gone"]; ok {
			t.Error("watch[Gone] should be gone")
		}
		lists := w.takeLists()
		if len(lists) != 1 || len(lists[0].Attrs) != 1 || lists[0].Attrs[0] != imaplib.MailboxAttrNonExistent {
			t.Errorf("delete LIST = %+v", lists)
		}
	})

	t.Run("rename carries OLDNAME and re-arms watch", func(t *testing.T) {
		w := newWatcher(personal, true, false)
		w.addSub("Old", notifyMaskNew)
		w.handleListEvent(locks.Event{Type: locks.EventMailboxRename, Payload: "Old" + renameSep + "New"})
		if _, ok := w.watch["Old"]; ok {
			t.Error("old name should be removed")
		}
		if w.watch["New"] != notifyMaskNew {
			t.Error("new name should be watched (PERSONAL)")
		}
		lists := w.takeLists()
		if len(lists) != 1 || lists[0].Mailbox != "New" || lists[0].OldName != "Old" {
			t.Errorf("rename LIST = %+v", lists)
		}
	})

	t.Run("subscribe with SUBSCRIBED filter adds watch and LIST", func(t *testing.T) {
		items := []imaplib.NotifyItem{
			{MailboxSpec: imaplib.NotifyMailboxSpecSubscribed, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMessageNew, imaplib.NotifyEventSubscriptionChange}},
		}
		w := newWatcher(items, false, true)
		w.handleListEvent(locks.Event{Type: locks.EventMailboxSubscribe, Payload: "Sub"})
		if w.watch["Sub"] != notifyMaskNew {
			t.Errorf("watch[Sub] = %d, want %d", w.watch["Sub"], notifyMaskNew)
		}
		lists := w.takeLists()
		if len(lists) != 1 || lists[0].Attrs[0] != imaplib.MailboxAttrSubscribed {
			t.Errorf("subscribe LIST = %+v", lists)
		}
	})

	t.Run("mailbox-level events suppressed when not requested", func(t *testing.T) {
		// PERSONAL with only MessageNew: create still grows the watch set but
		// emits no LIST (MailboxName not requested).
		items := []imaplib.NotifyItem{
			{MailboxSpec: imaplib.NotifyMailboxSpecPersonal, Events: []imaplib.NotifyEvent{imaplib.NotifyEventMessageNew}},
		}
		w := newWatcher(items, false, false)
		w.handleListEvent(locks.Event{Type: locks.EventMailboxCreate, Payload: "Silent"})
		if w.watch["Silent"] != notifyMaskNew {
			t.Error("watch should still grow for message events")
		}
		if lists := w.takeLists(); lists != nil {
			t.Errorf("no LIST expected, got %+v", lists)
		}
	})
}
