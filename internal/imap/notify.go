package imap

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/0kaba0hub/yarilo/pkg/locks"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Event masks for the non-selected NOTIFY watcher. A watched folder carries the
// OR of the message events the client requested for it; an incoming locks Event
// only marks the folder dirty when its type is in the mask.
const (
	notifyMaskNew     uint8 = 1 << iota // MessageNew   <- locks.EventDelivered
	notifyMaskExpunge                   // MessageExpunge <- locks.EventExpunged
	notifyMaskFlag                      // FlagChange   <- locks.EventChanged
)

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
// key; on a matching event the folder is marked dirty and Idle is woken. The
// pending set is drained into "* STATUS" responses by Poll (between commands)
// and Idle (while the client waits).
type notifyWatcher struct {
	mu    sync.Mutex
	dirty map[string]struct{} // folder names awaiting a STATUS response
	watch map[string]uint8    // folder name -> requested event mask
	wake  chan struct{}       // buffered(1) nudge for the Idle loop
	stop  context.CancelFunc
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

// eventMask folds the requested NOTIFY events into a message-event mask.
// Mailbox-level events (MailboxName, SubscriptionChange, …) are not handled in
// this phase and contribute nothing.
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

// startNotifyWatch builds the watched-folder set from the non-selected NOTIFY
// items and launches the subscription goroutines. Any previously running
// watcher must be stopped first. A no-op when no folder matches or the locker
// is unavailable (single-node without cross-process events still works for the
// selected mailbox via Poll).
func (s *session) startNotifyWatch(items []imaplib.NotifyItem) {
	if s.srv.opts.Locker == nil || s.userInfo == nil {
		return
	}
	watch := s.resolveNotifyWatch(items)
	if len(watch) == 0 {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &notifyWatcher{
		dirty: make(map[string]struct{}),
		watch: watch,
		wake:  make(chan struct{}, 1),
		stop:  cancel,
	}
	for folder := range watch {
		ch, err := s.srv.opts.Locker.Subscribe(ctx, locks.MailboxKey(s.userInfo.Username, folder))
		if err != nil {
			slog.Debug("imap: notify subscribe failed", "folder", folder, "err", err)
			continue
		}
		go func(folder string, ch <-chan locks.Event) {
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
		}(folder, ch)
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
		if s.subs == nil {
			return nil
		}
		snap, err := s.subs.Snapshot()
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(snap))
		for name := range snap {
			out = append(out, name)
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

// drainNotifyStatus flushes pending non-selected mailbox activity as untagged
// STATUS responses. Called by Poll (between commands) and Idle (while waiting).
func (s *session) drainNotifyStatus(w *imapserver.UpdateWriter) error {
	if s.notifyWatch == nil {
		return nil
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
