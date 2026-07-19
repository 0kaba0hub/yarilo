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
	"strings"
	"sync"
	"time"

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
		Driver:    info.Driver,
		Separator: info.Separator,
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
	id := nextJobID()
	slog.Debug("fts: index job queued", "job_id", id, "user", user, "folder", mbox.Name, "guid", mbox.GUID,
		"uidvalidity", mbox.UIDValidity, "max_uid", maxUID, "max_recent", maxRecent, "priority", false)
	s.queue.push(job{id: id, user: user, mbox: mbox, maxUID: maxUID, maxRecent: maxRecent}, false)
	return nil
}

func (s *Service) Prepend(user string, mbox fts.MailboxRef, maxUID uint32) error {
	id := nextJobID()
	slog.Debug("fts: index job queued", "job_id", id, "user", user, "folder", mbox.Name, "guid", mbox.GUID,
		"uidvalidity", mbox.UIDValidity, "max_uid", maxUID, "priority", true)
	s.queue.push(job{id: id, user: user, mbox: mbox, maxUID: maxUID}, true)
	return nil
}

func (s *Service) Expunge(user string, mbox fts.MailboxRef, uid uint32) error {
	h, err := s.handle(user)
	if err != nil {
		return err
	}
	err = s.opts.LockMailbox(user, mbox.Name, func() error {
		return h.ui.Expunge(mbox, uid)
	})
	slog.Debug("fts: expunge document", "user", user, "folder", mbox.Name, "uid", uid, "ok", err == nil)
	return err
}

func (s *Service) Lookup(user string, mbox fts.MailboxRef, q fts.Query) (fts.Result, error) {
	h, err := s.handle(user)
	if err != nil {
		return fts.Result{}, err
	}
	t0 := time.Now()
	res, err := h.ui.Lookup(mbox, q)
	// Term COUNT and result counts only — never the query terms (private content).
	slog.Debug("fts: lookup executed", "user", user, "folder", mbox.Name,
		"terms", len(q.Terms), "and_terms", q.AndTerms,
		"definite", len(res.Definite), "maybe", len(res.Maybe),
		"dur_ms", time.Since(t0).Milliseconds(), "err", err)
	return res, err
}

func (s *Service) Status(user string, mbox fts.MailboxRef) (uint32, uint32, error) {
	h, err := s.handle(user)
	if err != nil {
		return 0, 0, err
	}
	last, storedUIDV, sum, err := h.ui.Checkpoint(mbox)
	// A checkpoint recorded under a different UIDVALIDITY belongs to a mailbox that
	// has since been recreated — report "not indexed" (last=0) so the client's
	// catch-up queues a reindex that resets it, instead of trusting a stale
	// last_indexed_uid that suppresses indexing of the new low UIDs (#638).
	staleUIDV := last > 0 && mbox.UIDValidity != 0 && storedUIDV != mbox.UIDValidity
	if staleUIDV {
		last = 0
	}
	slog.Debug("fts: status", "user", user, "folder", mbox.Name,
		"last_indexed_uid", last, "settings_checksum", sum,
		"stored_uidvalidity", storedUIDV, "mbox_uidvalidity", mbox.UIDValidity, "stale_uidvalidity", staleUIDV, "err", err)
	return last, sum, err
}

func (s *Service) Rescan(user string, mbox fts.MailboxRef) error {
	h, err := s.handle(user)
	if err != nil {
		return err
	}
	present, maxUID, uidValidity, err := s.presentUIDs(h, mbox)
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
		if err := h.ui.SetCheckpoint(mbox, low-1, uidValidity, s.opts.Chain.SettingsChecksum()); err != nil {
			return err
		}
		rid := nextJobID()
		slog.Debug("fts: index job queued", "job_id", rid, "user", user, "folder", mbox.Name,
			"uidvalidity", uidValidity, "max_uid", maxUID, "priority", false, "source", "rescan")
		s.queue.push(job{id: rid, user: user, mbox: mbox, maxUID: maxUID}, false)
	}
	slog.Debug("fts: rescan reconciled", "user", user, "folder", mbox.Name,
		"present", len(present), "missing", len(missing), "max_uid", maxUID, "reindex_queued", len(missing) > 0)
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
				"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "err", err)
			// Recovery (#629): a broken/closed engine handle stays broken for every
			// subsequent job unless it is reopened. Drop the cached user handle so
			// the next job re-opens a fresh index — the engine also self-heals its
			// write shard, but evicting here recovers even a wholesale-poisoned
			// UserIndex without an operator deleting the on-disk index.
			if isBrokenEngine(err) {
				s.evict(j.user)
				slog.Warn("fts: engine reported a broken index, evicted user handle for reopen",
					"user", j.user, "folder", j.mbox.Name)
			}
		}
	}
}

// isBrokenEngine reports whether err indicates the engine's on-disk index or its
// open handle is in an unusable state — a Xapian DatabaseClosedError, or the
// rev-file open/write failure that wedges a flatcurve shard (#629). A false
// positive only costs a handle reopen, so the match is deliberately broad.
func isBrokenEngine(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"DatabaseClosedError",
		"Database has been closed",
		"DatabaseOpeningError",
		"Couldn't write new rev file",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// evict closes and drops the cached handle for user so the next handle() reopens
// a fresh index. Safe when no handle is cached.
func (s *Service) evict(user string) {
	s.mu.Lock()
	h, ok := s.users[user]
	if ok {
		delete(s.users, user)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = h.ui.Close()
	if h.box != nil {
		h.box.Close() //nolint:errcheck
	}
	if h.idx != nil {
		h.idx.Close() //nolint:errcheck
	}
}

func (s *Service) presentUIDs(h *userHandle, mbox fts.MailboxRef) (uids []uint32, maxUID, uidValidity uint32, err error) {
	folder, err := h.idx.OpenFolder(mbox.Name, mbox.UIDValidity)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("ftsservice: open folder: %w", err)
	}
	msgs, err := h.idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("ftsservice: list messages: %w", err)
	}
	uids = make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		uids = append(uids, m.UID)
		if m.UID > maxUID {
			maxUID = m.UID
		}
	}
	return uids, maxUID, folder.UIDValidity, nil
}

func (s *Service) runIndex(j job) error {
	h, err := s.handle(j.user)
	if err != nil {
		return err
	}
	tStart := time.Now()
	checksum := s.opts.Chain.SettingsChecksum()

	// Snapshot the folder (outside the lock): its UIDVALIDITY is the authoritative
	// current value — the Index/autoindex path often sends MailboxRef.UIDValidity=0,
	// so the checkpoint compare must use the folder's own value, not the job's.
	folder, err := h.idx.OpenFolder(j.mbox.Name, j.mbox.UIDValidity)
	if err != nil {
		return fmt.Errorf("ftsservice: open folder: %w", err)
	}
	curUIDV := folder.UIDValidity
	msgs, err := h.idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		return fmt.Errorf("ftsservice: list messages: %w", err)
	}

	indexedCount, skippedCount := 0, 0
	// Everything from the checkpoint read through the checkpoint write runs under
	// the per-mailbox lock (locks.MailboxKey(user, folder)): concurrent index jobs
	// for the SAME mailbox must not race the read-modify-write of last_indexed_uid
	// and clobber each other's progress. Different mailboxes/users are keyed
	// separately and index in parallel.
	err = s.opts.LockMailbox(j.user, j.mbox.Name, func() error {
		last, storedUIDV, storedSum, cerr := h.ui.Checkpoint(j.mbox)
		if cerr != nil {
			return cerr
		}
		// Decide whether the checkpoint is stale and the index must be rebuilt:
		//  - settings changed: query-time tokenization no longer matches the index;
		//  - UIDVALIDITY changed: the mailbox was recreated, so the stale
		//    last_indexed_uid can sit above the new low UIDs and silently suppress
		//    indexing of every new message (#638).
		reset := ""
		if last > 0 && storedSum != checksum {
			reset = "settings"
		} else if last > 0 && curUIDV != 0 && storedUIDV != curUIDV {
			reset = "uidvalidity"
		}
		slog.Debug("fts: index run start", "job_id", j.id, "user", j.user, "folder", j.mbox.Name,
			"checkpoint_uid", last, "target_max_uid", j.maxUID,
			"stored_checksum", storedSum, "current_checksum", checksum,
			"stored_uidvalidity", storedUIDV, "current_uidvalidity", curUIDV, "reset", reset)
		if reset != "" {
			slog.Info("fts: resetting mailbox index", "job_id", j.id, "user", j.user, "folder", j.mbox.Name, "reason", reset)
			if _, rerr := h.ui.Rescan(j.mbox, nil); rerr != nil { // drop every stale document
				return rerr
			}
			last = 0
		}
		if j.maxUID <= last {
			slog.Debug("fts: index run skipped, already current",
				"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "checkpoint_uid", last, "target_max_uid", j.maxUID)
			return nil
		}

		upd, err := h.ui.BeginUpdate(j.mbox)
		if err != nil {
			return err
		}
		indexed := last
		batch := 0
		marked := false // FSCKD flagged for this mailbox scan; gate repeat marks
		for _, m := range msgs {
			if m.UID <= last || m.UID > j.maxUID {
				continue
			}
			if err := s.indexOne(h, j.mbox, m, upd); err != nil {
				skippedCount++
				// One unreadable message must not stall the mailbox forever:
				// log and move the checkpoint past it (rescan can revisit).
				slog.Warn("fts: message skipped",
					"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "uid", m.UID, "err", err)
				// Flag the folder for a reactive heal once per scan (not per
				// message): a mailbox full of vanished files must not pay an
				// OpenFolder+mark for each one.
				if !marked && mailbox.MarkCorruptOnFetchErr(h.box, h.idx, j.mbox.Name, err) {
					marked = true
				}
			} else {
				indexedCount++
			}
			indexed = m.UID
			batch++
			if batch >= s.opts.CommitLimit {
				if err := upd.Commit(); err != nil {
					return err
				}
				if err := h.ui.SetCheckpoint(j.mbox, indexed, curUIDV, checksum); err != nil {
					return err
				}
				batch = 0
			}
		}
		if err := upd.Commit(); err != nil {
			return err
		}
		if indexed != last {
			return h.ui.SetCheckpoint(j.mbox, indexed, curUIDV, checksum)
		}
		return nil
	})
	slog.Debug("fts: index run done", "job_id", j.id, "user", j.user, "folder", j.mbox.Name,
		"messages_in_folder", len(msgs), "indexed", indexedCount, "skipped", skippedCount,
		"dur_ms", time.Since(tStart).Milliseconds(), "err", err)
	return err
}

func (s *Service) indexOne(h *userHandle, mbox fts.MailboxRef, m *mailbox.MessageMeta, upd fts.Update) error {
	rc, err := h.box.Fetch(mbox.Name, m.Filename, m.AltTier)
	if err != nil {
		// The caller flags the folder for a reactive heal (gated once per scan);
		// here we just surface the read error.
		return err
	}
	defer rc.Close()
	if err := s.builder.Build(m.UID, io.Reader(rc), upd); err != nil {
		return err
	}
	// Per-message breadcrumb: which UID/file was fed to the engine (size is the
	// index-time signal for "was there anything to tokenize"). Metadata only.
	slog.Debug("fts: message indexed", "folder", mbox.Name, "guid", mbox.GUID,
		"uid", m.UID, "file", m.Filename, "size", m.Size, "alt_tier", m.AltTier)
	return nil
}
