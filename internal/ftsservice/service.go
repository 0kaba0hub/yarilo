// Package ftsservice is the yarilo-fts core: the sole owner of the FTS
// indexes (single writer + lookup endpoint, docs/FTS.md §4), an indexing
// queue with priority inserts for on-demand search catch-up, and the worker
// that streams messages through buildmail into the engine.
package ftsservice

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/0kaba0hub/yarilo/internal/fts/buildmail"
	"github.com/0kaba0hub/yarilo/internal/fts/language"
	"github.com/0kaba0hub/yarilo/pkg/fts"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Options wires the service dependencies.
type Options struct {
	Engine  fts.Engine
	Mailbox mailbox.MailboxBackend
	Index   mailbox.IndexBackend
	// ResolveUser maps a username to its storage identity (userdb).
	ResolveUser func(username string) (*mailbox.UserInfo, error)
	Chain       *language.Chain
	Build       buildmail.Options
	// CommitLimit batches engine commits during an index walk (default 500).
	CommitLimit int
	// Workers is the number of concurrent index workers (default 1).
	Workers int
	// LockMailbox wraps every index write in the cross-process mailbox lock
	// (pkg/locks; wired by the binary). nil = direct call (unit tests only).
	LockMailbox func(user, folder string, fn func() error) error

	// MailboxByDriver returns the mailbox backend for a per-user storage
	// driver (mdbox / sdbox / maildir) when it differs from the global
	// Mailbox — the userdb mail_location driver, resolved exactly as the
	// session pods do. nil, or a nil result, falls back to Mailbox.
	MailboxByDriver func(driver string) mailbox.MailboxBackend
}

// Service implements ftsproto.Service.
type Service struct {
	opts    Options
	builder *buildmail.Builder
	queue   *queue

	mu    sync.Mutex
	users map[string]*userHandle

	wg   sync.WaitGroup
	stop context.CancelFunc
}

type userHandle struct {
	info *mailbox.UserInfo
	ui   fts.UserIndex
	box  mailbox.UserMailbox
	idx  mailbox.UserIndex
}

// New builds the service and starts its workers.
func New(opts Options) (*Service, error) {
	if opts.Engine == nil || opts.Mailbox == nil || opts.Index == nil ||
		opts.ResolveUser == nil || opts.Chain == nil {
		return nil, fmt.Errorf("ftsservice: incomplete options")
	}
	if opts.CommitLimit <= 0 {
		opts.CommitLimit = 500
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.LockMailbox == nil {
		opts.LockMailbox = func(_, _ string, fn func() error) error { return fn() }
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		opts:    opts,
		builder: buildmail.New(opts.Build, opts.Chain),
		queue:   newQueue(),
		users:   map[string]*userHandle{},
		stop:    cancel,
	}
	for i := 0; i < opts.Workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx)
	}
	return s, nil
}

// Close drains the workers and closes every open user index.
func (s *Service) Close() error {
	s.stop()
	s.queue.close()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for _, h := range s.users {
		if err := h.ui.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if h.box != nil {
			h.box.Close() //nolint:errcheck
		}
		if h.idx != nil {
			h.idx.Close() //nolint:errcheck
		}
	}
	s.users = map[string]*userHandle{}
	return firstErr
}

func (s *Service) handle(user string) (*userHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.users[user]; ok {
		return h, nil
	}
	info, err := s.opts.ResolveUser(user)
	if err != nil {
		return nil, fmt.Errorf("ftsservice: userdb %s: %w", user, err)
	}
	ui, err := s.opts.Engine.OpenUser(context.Background(), fts.UserRef{
		Username:  user,
		IndexRoot: indexRoot(info),
	})
	if err != nil {
		return nil, err
	}
	h := &userHandle{
		info: info,
		ui:   ui,
		box:  s.mailboxFor(info).OpenUser(info),
		idx:  s.opts.Index.OpenUser(info),
	}
	s.users[user] = h
	return h, nil
}

// mailboxFor selects the backend matching the user's storage driver, falling
// back to the global Mailbox when no per-driver factory is wired or the driver
// is the global default.
func (s *Service) mailboxFor(info *mailbox.UserInfo) mailbox.MailboxBackend {
	if info.Driver != "" && s.opts.MailboxByDriver != nil {
		if mb := s.opts.MailboxByDriver(info.Driver); mb != nil {
			return mb
		}
	}
	return s.opts.Mailbox
}

// indexRoot mirrors the fileindex root resolution: INDEX= override → mail
// path → home.
func indexRoot(info *mailbox.UserInfo) string {
	if info.IndexDir != "" {
		return info.IndexDir
	}
	if info.MailPath != "" {
		return info.MailPath
	}
	return info.Home
}

/* --- ftsproto.Service ------------------------------------------------------ */

func (s *Service) Index(user string, mbox fts.MailboxRef, maxUID uint32, maxRecent int) error {
	s.queue.push(job{user: user, mbox: mbox, maxUID: maxUID, maxRecent: maxRecent}, false)
	return nil
}

func (s *Service) Prepend(user string, mbox fts.MailboxRef, maxUID uint32) error {
	s.queue.push(job{user: user, mbox: mbox, maxUID: maxUID}, true)
	return nil
}

func (s *Service) Expunge(user string, mbox fts.MailboxRef, uid uint32) error {
	h, err := s.handle(user)
	if err != nil {
		return err
	}
	return s.opts.LockMailbox(user, mbox.Name, func() error {
		return h.ui.Expunge(mbox, uid)
	})
}

func (s *Service) Lookup(user string, mbox fts.MailboxRef, q fts.Query) (fts.Result, error) {
	h, err := s.handle(user)
	if err != nil {
		return fts.Result{}, err
	}
	return h.ui.Lookup(mbox, q)
}

func (s *Service) Status(user string, mbox fts.MailboxRef) (uint32, uint32, error) {
	h, err := s.handle(user)
	if err != nil {
		return 0, 0, err
	}
	return h.ui.Checkpoint(mbox)
}

func (s *Service) Rescan(user string, mbox fts.MailboxRef) error {
	h, err := s.handle(user)
	if err != nil {
		return err
	}
	present, maxUID, err := s.presentUIDs(h, mbox)
	if err != nil {
		return err
	}
	var missing []uint32
	if err := s.opts.LockMailbox(user, mbox.Name, func() error {
		var rerr error
		missing, rerr = h.ui.Rescan(mbox, present)
		return rerr
	}); err != nil {
		return err
	}
	if len(missing) > 0 {
		// The checkpoint may sit above the gaps; reset it so the index walk
		// revisits the missing range.
		low := missing[0]
		if err := h.ui.SetCheckpoint(mbox, low-1, s.opts.Chain.SettingsChecksum()); err != nil {
			return err
		}
		s.queue.push(job{user: user, mbox: mbox, maxUID: maxUID}, false)
	}
	return nil
}

func (s *Service) Optimize(user string) error {
	h, err := s.handle(user)
	if err != nil {
		return err
	}
	return s.opts.LockMailbox(user, "", func() error {
		return h.ui.Optimize()
	})
}

/* --- worker ----------------------------------------------------------------- */

func (s *Service) worker(ctx context.Context) {
	defer s.wg.Done()
	for {
		j, ok := s.queue.pop(ctx)
		if !ok {
			return
		}
		if err := s.runIndex(j); err != nil {
			slog.Error("fts: index job failed",
				"user", j.user, "folder", j.mbox.Name, "err", err)
		}
	}
}

func (s *Service) presentUIDs(h *userHandle, mbox fts.MailboxRef) ([]uint32, uint32, error) {
	folder, err := h.idx.OpenFolder(mbox.Name, mbox.UIDValidity)
	if err != nil {
		return nil, 0, fmt.Errorf("ftsservice: open folder: %w", err)
	}
	msgs, err := h.idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, 0, fmt.Errorf("ftsservice: list messages: %w", err)
	}
	uids := make([]uint32, 0, len(msgs))
	var maxUID uint32
	for _, m := range msgs {
		uids = append(uids, m.UID)
		if m.UID > maxUID {
			maxUID = m.UID
		}
	}
	return uids, maxUID, nil
}

func (s *Service) runIndex(j job) error {
	h, err := s.handle(j.user)
	if err != nil {
		return err
	}
	checksum := s.opts.Chain.SettingsChecksum()
	last, storedSum, err := h.ui.Checkpoint(j.mbox)
	if err != nil {
		return err
	}
	if last > 0 && storedSum != checksum {
		// Tokenizer/filter config changed: this mailbox's index no longer
		// matches query-time tokenization — rebuild it from scratch.
		slog.Info("fts: settings changed, rebuilding mailbox",
			"user", j.user, "folder", j.mbox.Name)
		if err := s.opts.LockMailbox(j.user, j.mbox.Name, func() error {
			_, rerr := h.ui.Rescan(j.mbox, nil) // drop every stale document
			return rerr
		}); err != nil {
			return err
		}
		last = 0
	}
	if j.maxUID <= last {
		return nil
	}

	folder, err := h.idx.OpenFolder(j.mbox.Name, j.mbox.UIDValidity)
	if err != nil {
		return fmt.Errorf("ftsservice: open folder: %w", err)
	}
	msgs, err := h.idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		return fmt.Errorf("ftsservice: list messages: %w", err)
	}

	return s.opts.LockMailbox(j.user, j.mbox.Name, func() error {
		upd, err := h.ui.BeginUpdate(j.mbox)
		if err != nil {
			return err
		}
		indexed := last
		batch := 0
		for _, m := range msgs {
			if m.UID <= last || m.UID > j.maxUID {
				continue
			}
			if err := s.indexOne(h, j.mbox, m, upd); err != nil {
				// One unreadable message must not stall the mailbox forever:
				// log and move the checkpoint past it (rescan can revisit).
				slog.Warn("fts: message skipped",
					"user", j.user, "folder", j.mbox.Name, "uid", m.UID, "err", err)
			}
			indexed = m.UID
			batch++
			if batch >= s.opts.CommitLimit {
				if err := upd.Commit(); err != nil {
					return err
				}
				if err := h.ui.SetCheckpoint(j.mbox, indexed, checksum); err != nil {
					return err
				}
				batch = 0
			}
		}
		if err := upd.Commit(); err != nil {
			return err
		}
		if indexed != last {
			return h.ui.SetCheckpoint(j.mbox, indexed, checksum)
		}
		return nil
	})
}

func (s *Service) indexOne(h *userHandle, mbox fts.MailboxRef, m *mailbox.MessageMeta, upd fts.Update) error {
	rc, err := h.box.Fetch(mbox.Name, m.Filename, m.AltTier)
	if err != nil {
		return err
	}
	defer rc.Close()
	return s.builder.Build(m.UID, io.Reader(rc), upd)
}
