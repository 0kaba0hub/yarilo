package jmap

import (
	"fmt"
	"log/slog"

	"github.com/yarilomail/yarilo/internal/userstate/specialuse"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Storage wires everything a request needs to reach one user's mail. It mirrors
// the session protocols: userdb resolves the storage identity, then the backends
// hand out per-user handles.
type Storage struct {
	Mailbox mailbox.MailboxBackend
	Index   mailbox.IndexBackend
	// ResolveUser maps a username to its storage identity (userdb).
	ResolveUser func(username string) (*mailbox.UserInfo, error)
	// MailboxByDriver returns the backend for a per-user storage driver when it
	// differs from the global one, as the other services do.
	MailboxByDriver func(driver string) mailbox.MailboxBackend
	Locker          locks.Locker
	// Threads is the shared fold cache for the account threading sidecars.
	// Nil disables threading, and the account reads as it did before the
	// feature: every message its own conversation (#1425).
	Threads *threads.Cache
	// SpecialUseDefaults maps folder name to its attribute, from
	// protocol.imap.imap_special_use_defaults. Per-user overrides win.
	SpecialUseDefaults map[string]string
}

// userHandle is one request's view of a user's mail. JMAP has no session, so a
// handle lives for one request and is closed with it: a cache would need an
// invalidation story that nothing here can supply.
type userHandle struct {
	info       *mailbox.UserInfo
	threads    *threads.Cache
	box        mailbox.UserMailbox
	idx        mailbox.UserIndex
	subs       *subs.Store
	specialUse *specialuse.Store
}

func (h *userHandle) close() {
	if h.box != nil {
		if err := h.box.Close(); err != nil {
			slog.Debug("jmap: mailbox close failed", "err", err)
		}
	}
	if h.idx != nil {
		if err := h.idx.Close(); err != nil {
			slog.Debug("jmap: index close failed", "err", err)
		}
	}
}

// open resolves the user and opens their handles.
func (s *Storage) open(username, sessionID string) (*userHandle, error) {
	if s == nil || s.ResolveUser == nil || s.Mailbox == nil || s.Index == nil {
		return nil, fmt.Errorf("jmap: storage is not wired")
	}
	// A nil locker degrades the control-file reads to unlocked ones rather than
	// failing, so a torn read against a concurrent IMAP SUBSCRIBE or
	// CREATE (USE ...) would look like a correct answer. Refuse instead.
	if s.Locker == nil {
		return nil, fmt.Errorf("jmap: no locks client — subscription and special-use reads would not be consistent against a live IMAP session")
	}
	info, err := s.ResolveUser(username)
	if err != nil {
		return nil, fmt.Errorf("jmap: userdb %s: %w", username, err)
	}
	if sessionID != "" {
		info.SessionID = sessionID
	}
	// The session's own name on every lock this request takes. It used to pass
	// the bare username as the owner: one segment, no process, no request
	// (#1670).
	owner := locks.Owner(username, info.LockID())
	h := &userHandle{
		info: info,
		box:  s.mailboxFor(info).OpenUser(info),
		idx:  s.Index.OpenUser(info),
	}
	h.threads = s.Threads
	h.subs = subs.New(controlRoot(info), subsFile, username, owner, s.Locker)
	h.specialUse = specialuse.New(info.Home, username, owner, s.Locker, s.SpecialUseDefaults)
	return h, nil
}

// subsFile is the personal namespace's subscription list. JMAP exposes the
// personal namespace only until the namespace phase, and this is the same file
// IMAP reads, so a folder subscribed over one protocol is subscribed over both.
const subsFile = "subscriptions"

// controlRoot is where per-user control files live: ControlDir when set, else
// MailPath, falling back to Home. IMAP resolves it the same way, and reading a
// different root would show a user a different subscription list per protocol.
func controlRoot(info *mailbox.UserInfo) string {
	return mailbox.ControlRoot(info)
}

// mailboxFor selects the backend matching the user's storage driver, falling
// back to the global one — the same resolution the other services use.
func (s *Storage) mailboxFor(info *mailbox.UserInfo) mailbox.MailboxBackend {
	if info.Driver != "" && s.MailboxByDriver != nil {
		if mb := s.MailboxByDriver(info.Driver); mb != nil {
			return mb
		}
	}
	return s.Mailbox
}

// lazyStore defers opening the user's mail until a method asks for it. A batch
// of methods that never touch storage must not fail because storage is down,
// and Core/echo — whose whole purpose is to answer when other things do not —
// must never be the first thing a broken dependency takes out.
type lazyStore struct {
	// sessionID is the login hop's id for this request, and what its locks
	// announce (#1670).
	sessionID string

	storage *Storage
	user    string
	handle  *userHandle
	err     error
	done    bool
}

// get opens the handle once per request. A failure is remembered, so a batch of
// several storage-backed calls reports it once per call without retrying a
// dependency that is already known to be down.
func (l *lazyStore) get() (*userHandle, error) {
	if l.done {
		return l.handle, l.err
	}
	l.done = true
	if l.storage == nil {
		l.err = fmt.Errorf("jmap: no mail store configured")
		return nil, l.err
	}
	l.handle, l.err = l.storage.open(l.user, l.sessionID)
	return l.handle, l.err
}

func (l *lazyStore) close() {
	if l.handle != nil {
		l.handle.close()
	}
}
