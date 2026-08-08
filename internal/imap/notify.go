package imap

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Event masks for the non-selected NOTIFY watcher. A watched folder carries the
// OR of the message events the client requested for it; an incoming locks Event
// only marks the folder dirty when its type is in the mask.
const (
	notifyMaskNew     uint8 = 1 << iota // MessageNew   <- locks.EventDelivered
	notifyMaskExpunge                   // MessageExpunge <- locks.EventExpunged
	notifyMaskFlag                      // FlagChange   <- locks.EventChanged
)

// notifyDelim is the hierarchy separator reported in NOTIFY LIST responses.
const notifyDelim = '/'

// renameSep joins the old and new names in a rename event payload. It must be a
// byte that never appears in a mailbox name and survives the locks wire protocol
// (which is TAB/LF-framed and rejects those two). NUL satisfies both.
const renameSep = "\x00"

// notifyStatusOpts is the fixed set of STATUS items reported for non-selected
// mailbox activity (RFC 5465 §6): enough for a client to detect new/removed
// mail and resync CONDSTORE state without selecting the mailbox.
var notifyStatusOpts = &imaplib.StatusOptions{
	NumMessages:   true,
	UIDNext:       true,
	UIDValidity:   true,
	NumUnseen:     true,
	HighestModSeq: true,
}

// notifyWatcher tracks activity in non-selected mailboxes for an active
// NOTIFY SET (RFC 5465). It subscribes to each watched folder's locks event
// key (message activity -> "* STATUS") and to the user's mailbox-list key
// (create / delete / rename / subscribe -> dynamic re-evaluation and, when the
// client asked for MailboxName / SubscriptionChange, "* LIST" responses).
//
// The watch set starts from the filters resolved at NOTIFY SET and grows or
// shrinks as mailboxes appear and disappear. Poll (between commands) and Idle
// (while the client waits) drain the pending notifications.
type notifyWatcher struct {
	mu         sync.Mutex
	dirty      map[string]struct{} // folders awaiting a STATUS response
	watch      map[string]uint8    // folder -> active event mask
	subscribed map[string]struct{} // folders with a live mbox-key subscription
	pending    []imaplib.ListData  // queued LIST notifications (RFC 5465 §5)

	// items and the two want* flags are captured at NOTIFY SET and are
	// immutable afterwards, so they need no lock.
	items    []imaplib.NotifyItem
	wantName bool // MailboxName requested (create / delete / rename)
	wantSub  bool // SubscriptionChange requested (subscribe / unsubscribe)

	wake chan struct{}      // buffered(1) nudge for the Idle loop
	stop context.CancelFunc // cancels every subscription goroutine

	// addSub subscribes to a folder's mbox event key (once) and OR-s mask into
	// its watch entry. Set at start so handleListEvent can grow the set.
	addSub func(folder string, mask uint8)
}

// mark flags a folder dirty when evt matches its requested mask and nudges Idle.
func (n *notifyWatcher) mark(folder string, t locks.EventType) {
	var bit uint8
	switch t {
	case locks.EventDelivered:
		bit = notifyMaskNew
	case locks.EventExpunged:
		bit = notifyMaskExpunge
	case locks.EventChanged:
		bit = notifyMaskFlag
	default:
		return
	}
	n.mu.Lock()
	if n.watch[folder]&bit == 0 {
		n.mu.Unlock()
		return
	}
	n.dirty[folder] = struct{}{}
	n.mu.Unlock()
	n.nudge()
}

// nudge signals the Idle loop that work is pending (non-blocking).
func (n *notifyWatcher) nudge() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// take returns and clears the pending dirty folder names.
func (n *notifyWatcher) take() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.dirty) == 0 {
		return nil
	}
	out := make([]string, 0, len(n.dirty))
	for f := range n.dirty {
		out = append(out, f)
	}
	n.dirty = make(map[string]struct{})
	return out
}

// takeLists returns and clears the queued LIST notifications.
func (n *notifyWatcher) takeLists() []imaplib.ListData {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.pending) == 0 {
		return nil
	}
	out := n.pending
	n.pending = nil
	return out
}

// queueList enqueues a LIST notification and nudges Idle.
func (n *notifyWatcher) queueList(data imaplib.ListData) {
	n.mu.Lock()
	n.pending = append(n.pending, data)
	n.mu.Unlock()
	n.nudge()
}

// removeWatch stops tracking a folder (deleted or unsubscribed out of scope).
// The folder's subscription goroutine stays alive but is inert: mark() ignores
// a zero mask. If the folder reappears, addSub re-arms the mask without a new
// subscription.
func (n *notifyWatcher) removeWatch(folder string) {
	n.mu.Lock()
	delete(n.watch, folder)
	delete(n.dirty, folder)
	n.mu.Unlock()
}

// matchStaticFilters returns the OR of masks from every non-selected,
// non-SUBSCRIBED filter that names folder. Used to decide whether a
// newly-created or renamed mailbox joins the watch set.
func (n *notifyWatcher) matchStaticFilters(folder string) uint8 {
	var mask uint8
	for _, it := range n.items {
		switch it.MailboxSpec {
		case imaplib.NotifyMailboxSpecSelected, imaplib.NotifyMailboxSpecSelectedDelayed,
			imaplib.NotifyMailboxSpecSubscribed:
			continue
		case imaplib.NotifyMailboxSpecPersonal:
			mask |= eventMask(it.Events)
		case imaplib.NotifyMailboxSpecInboxes:
			if folder == "INBOX" || strings.HasPrefix(folder, "INBOX/") {
				mask |= eventMask(it.Events)
			}
		default:
			em := eventMask(it.Events)
			for _, base := range it.Mailboxes {
				if folder == base || (it.Subtree && strings.HasPrefix(folder, base+"/")) {
					mask |= em
				}
			}
		}
	}
	return mask
}

// subscribedMask returns the OR of masks from SUBSCRIBED filters (0 if none).
func (n *notifyWatcher) subscribedMask() uint8 {
	var mask uint8
	for _, it := range n.items {
		if it.MailboxSpec == imaplib.NotifyMailboxSpecSubscribed {
			mask |= eventMask(it.Events)
		}
	}
	return mask
}

// handleListEvent applies a mailbox-list event: it grows/shrinks the watch set
// and queues LIST notifications when the client requested MailboxName /
// SubscriptionChange.
func (n *notifyWatcher) handleListEvent(evt locks.Event) {
	switch evt.Type {
	case locks.EventMailboxCreate:
		if m := n.matchStaticFilters(evt.Payload); m != 0 {
			n.addSub(evt.Payload, m)
		}
		if n.wantName {
			n.queueList(imaplib.ListData{Mailbox: evt.Payload, Delim: notifyDelim})
		}
	case locks.EventMailboxDelete:
		n.removeWatch(evt.Payload)
		if n.wantName {
			n.queueList(imaplib.ListData{
				Mailbox: evt.Payload, Delim: notifyDelim,
				Attrs: []imaplib.MailboxAttr{imaplib.MailboxAttrNonExistent},
			})
		}
	case locks.EventMailboxRename:
		oldName, newName, ok := strings.Cut(evt.Payload, renameSep)
		if !ok {
			return
		}
		n.removeWatch(oldName)
		if m := n.matchStaticFilters(newName); m != 0 {
			n.addSub(newName, m)
		}
		if n.wantName {
			n.queueList(imaplib.ListData{Mailbox: newName, Delim: notifyDelim, OldName: oldName})
		}
	case locks.EventMailboxSubscribe:
		if m := n.subscribedMask(); m != 0 {
			n.addSub(evt.Payload, m)
		}
		if n.wantSub {
			n.queueList(imaplib.ListData{
				Mailbox: evt.Payload, Delim: notifyDelim,
				Attrs: []imaplib.MailboxAttr{imaplib.MailboxAttrSubscribed},
			})
		}
	case locks.EventMailboxUnsubscribe:
		// Drop only when the folder was watched purely because it was
		// subscribed — a static filter (PERSONAL / SUBTREE / …) keeps it.
		if n.subscribedMask() != 0 && n.matchStaticFilters(evt.Payload) == 0 {
			n.removeWatch(evt.Payload)
		}
		if n.wantSub {
			n.queueList(imaplib.ListData{Mailbox: evt.Payload, Delim: notifyDelim})
		}
	}
}

// eventMask folds the requested NOTIFY message events into a mask. Mailbox-level
// events (MailboxName, SubscriptionChange, …) contribute nothing here — they are
// tracked separately via wantName / wantSub.
func eventMask(events []imaplib.NotifyEvent) uint8 {
	var m uint8
	for _, e := range events {
		switch e {
		case imaplib.NotifyEventMessageNew:
			m |= notifyMaskNew
		case imaplib.NotifyEventMessageExpunge:
			m |= notifyMaskExpunge
		case imaplib.NotifyEventFlagChange:
			m |= notifyMaskFlag
		}
	}
	return m
}

// wantsEvent reports whether any non-selected item lists the given event.
func wantsEvent(items []imaplib.NotifyItem, want imaplib.NotifyEvent) bool {
	for _, it := range items {
		switch it.MailboxSpec {
		case imaplib.NotifyMailboxSpecSelected, imaplib.NotifyMailboxSpecSelectedDelayed:
			continue
		}
		for _, e := range it.Events {
			if e == want {
				return true
			}
		}
	}
	return false
}

// startNotifyWatch builds the watched-folder set from the non-selected NOTIFY
// items and launches the subscription goroutines. Any previously running
// watcher must be stopped first. A no-op when the locker is unavailable or no
// non-selected filter / mailbox-level event was requested.
func (s *session) startNotifyWatch(items []imaplib.NotifyItem) {
	if s.srv.opts.Locker == nil || s.userInfo == nil {
		return
	}
	static := s.resolveNotifyWatch(items)
	wantName := wantsEvent(items, imaplib.NotifyEventMailboxName)
	wantSub := wantsEvent(items, imaplib.NotifyEventSubscriptionChange)
	// Something to watch? Either message activity in some mailbox, or a
	// mailbox-list event class. A message filter with no folders yet still
	// arms the list watcher so future creates are caught.
	hasMsgFilter := false
	for _, it := range items {
		switch it.MailboxSpec {
		case imaplib.NotifyMailboxSpecSelected, imaplib.NotifyMailboxSpecSelectedDelayed:
			continue
		}
		if eventMask(it.Events) != 0 {
			hasMsgFilter = true
		}
	}
	if !hasMsgFilter && !wantName && !wantSub {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &notifyWatcher{
		dirty:      make(map[string]struct{}),
		watch:      make(map[string]uint8),
		subscribed: make(map[string]struct{}),
		items:      items,
		wantName:   wantName,
		wantSub:    wantSub,
		wake:       make(chan struct{}, 1),
		stop:       cancel,
	}
	w.addSub = func(folder string, mask uint8) {
		w.mu.Lock()
		w.watch[folder] |= mask
		if _, dup := w.subscribed[folder]; dup {
			w.mu.Unlock()
			return
		}
		w.subscribed[folder] = struct{}{}
		w.mu.Unlock()

		ch, err := s.srv.opts.Locker.Subscribe(ctx, locks.MailboxKey(s.userInfo.Username, folder))
		if err != nil {
			slog.Debug("imap: notify subscribe failed", "folder", folder, "err", err)
			return
		}
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-ch:
					if !ok {
						return
					}
					w.mark(folder, evt.Type)
				}
			}
		}()
	}

	for folder, mask := range static {
		w.addSub(folder, mask)
	}

	// Mailbox-list subscription drives dynamic membership and MailboxName /
	// SubscriptionChange notifications.
	if listCh, err := s.srv.opts.Locker.Subscribe(ctx, locks.MailboxListKey(s.userInfo.Username)); err != nil {
		slog.Debug("imap: notify list subscribe failed", "err", err)
	} else {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-listCh:
					if !ok {
						return
					}
					w.handleListEvent(evt)
				}
			}
		}()
	}

	s.notifyWatch = w
}

// stopNotifyWatch cancels the current watcher, if any.
func (s *session) stopNotifyWatch() {
	if s.notifyWatch != nil {
		s.notifyWatch.stop()
		s.notifyWatch = nil
	}
}

// resolveNotifyWatch expands the non-selected NOTIFY items into a folder->mask
// map. The selected mailbox is excluded: its events flow through the existing
// EXISTS/EXPUNGE/FETCH path, not STATUS.
func (s *session) resolveNotifyWatch(items []imaplib.NotifyItem) map[string]uint8 {
	watch := make(map[string]uint8)
	var allFolders []string // lazily populated personal-namespace listing

	list := func() []string {
		if allFolders == nil {
			allFolders = []string{}
			if entries, err := s.primary.box.ListFolders(); err == nil {
				allFolders = mailbox.SelectableNames(entries)
			}
		}
		return allFolders
	}

	for _, it := range items {
		switch it.MailboxSpec {
		case imaplib.NotifyMailboxSpecSelected, imaplib.NotifyMailboxSpecSelectedDelayed:
			continue
		}
		mask := eventMask(it.Events)
		if mask == 0 {
			continue
		}
		for _, name := range s.expandNotifySpec(it, list) {
			if s.folder != nil && name == s.folder.Name {
				continue
			}
			watch[name] |= mask
		}
	}
	return watch
}

// expandNotifySpec resolves a single NOTIFY item to concrete folder names.
// list lazily yields the personal namespace listing so PERSONAL/SUBTREE/INBOXES
// walks share one directory read.
func (s *session) expandNotifySpec(it imaplib.NotifyItem, list func() []string) []string {
	switch it.MailboxSpec {
	case imaplib.NotifyMailboxSpecPersonal:
		return list()
	case imaplib.NotifyMailboxSpecInboxes:
		return append([]string{"INBOX"}, descendants("INBOX", list())...)
	case imaplib.NotifyMailboxSpecSubscribed:
		// These names are compared against folder names relative to a handle,
		// so the set must hold personal-namespace names -- and the store's keys
		// are client-visible names now that subscriptions follow the subscriber
		// (user/alice/Sent, Public/Foo live in this same file). Before that the
		// file held only personal names, so reading it raw was correct by
		// accident; taking the keys as personal folders would put another
		// namespace's mailbox into the watch set under a name the personal
		// namespace does not have.
		store, keyPrefix, err := s.subsView(s.primary)
		if err != nil {
			return nil
		}
		snap, serr := store.Snapshot()
		if serr != nil {
			return nil
		}
		out := make([]string, 0, len(snap))
		for key := range snap {
			rel, ok := strings.CutPrefix(key, keyPrefix)
			if !ok || !s.namesPrimaryFolder(rel) {
				continue
			}
			out = append(out, rel)
		}
		return out
	default:
		// Explicit mailbox list (MAILBOXES / SUBTREE): the fork leaves
		// MailboxSpec empty and fills Mailboxes.
		out := make([]string, 0, len(it.Mailboxes))
		for _, base := range it.Mailboxes {
			out = append(out, base)
			if it.Subtree {
				out = append(out, descendants(base, list())...)
			}
		}
		return out
	}
}

// descendants returns the names in all that live under base ("base/…").
func descendants(base string, all []string) []string {
	prefix := base + "/"
	var out []string
	for _, name := range all {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	return out
}

// drainNotifyStatus flushes pending non-selected mailbox activity: first the
// LIST notifications (RFC 5465 §5), then the STATUS responses (§6). Called by
// Poll (between commands) and Idle (while waiting).
func (s *session) drainNotifyStatus(w *imapserver.UpdateWriter) error {
	if s.notifyWatch == nil {
		return nil
	}
	for _, ld := range s.notifyWatch.takeLists() {
		data := ld
		if err := w.WriteList(&data); err != nil {
			return err
		}
	}
	for _, folder := range s.notifyWatch.take() {
		data, err := s.Status(folder, notifyStatusOpts)
		if err != nil {
			// Folder vanished or unreadable — skip; the client re-syncs on
			// its next explicit STATUS/SELECT.
			slog.Debug("imap: notify status failed", "folder", folder, "err", err)
			continue
		}
		if err := w.WriteStatus(data, notifyStatusOpts); err != nil {
			return err
		}
	}
	return nil
}
