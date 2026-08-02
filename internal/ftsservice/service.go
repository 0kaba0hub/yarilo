// Package ftsservice is the yarilo-fts core: the sole owner of the FTS
// indexes (single writer + lookup endpoint, docs/FTS.md §4), an indexing
// queue with priority inserts for on-demand search catch-up, and the worker
// that streams messages through buildmail into the engine.
package ftsservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/fts/buildmail"
	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Options wires the service dependencies.
type Options struct {
	Engine  fts.Engine
	Mailbox mailbox.MailboxBackend
	Index   mailbox.IndexBackend
	// ResolveUser maps a username to its storage identity (userdb).
	ResolveUser func(username string) (*mailbox.UserInfo, error)
	Chain       *language.MultiChain
	Build       buildmail.Options
	// CommitLimit batches engine commits during an index walk (default 500).
	CommitLimit int
	// Workers is the number of concurrent index workers (default 1).
	Workers int
	// LockMailbox wraps every index write in the cross-process mailbox lock
	// (pkg/locks; wired by the binary). nil = direct call (unit tests only).
	LockMailbox func(user, folder string, fn func() error) error

	// MailboxByDriver returns the mailbox backend for a per-user storage driver
	// (mdbox / sdbox / maildir) when it differs from the global Mailbox — the
	// userdb mail_location driver, resolved as the session pods do. nil, or a
	// nil result, falls back to Mailbox.
	MailboxByDriver func(driver string) mailbox.MailboxBackend
}

// Service implements ftsproto.Service.
type Service struct {
	opts          Options
	builder       *buildmail.Builder
	queue         *queue
	optimizeQueue *optimizeQueue

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
		opts:          opts,
		builder:       buildmail.New(opts.Build, opts.Chain),
		queue:         newQueue(),
		optimizeQueue: newOptimizeQueue(),
		users:         map[string]*userHandle{},
		stop:          cancel,
	}
	// Wired before any worker starts, so the field write inside
	// SetOptimizeCallback happens-before every goroutine that could read it. An
	// engine that doesn't grow shards unboundedly simply doesn't implement
	// OptimizeNotifier.
	if on, ok := opts.Engine.(fts.OptimizeNotifier); ok {
		on.SetOptimizeCallback(s.enqueueOptimize)
	}
	for i := 0; i < opts.Workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx)
	}
	s.wg.Add(1)
	go s.optimizeWorker(ctx)
	return s, nil
}

// Close drains the workers and closes every open user index.
func (s *Service) Close() error {
	s.stop()
	s.queue.close()
	s.optimizeQueue.close()
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
	metricLookupTotal.Inc()
	h, err := s.handle(user)
	if err != nil {
		metricLookupErrors.Inc()
		return fts.Result{}, err
	}
	t0 := time.Now()
	res, err := h.ui.Lookup(mbox, q)
	metricLookupDuration.Observe(time.Since(t0).Seconds())
	if err != nil {
		metricLookupErrors.Inc()
	} else {
		metricLookupCandidates.Observe(float64(len(res.Definite) + len(res.Maybe)))
	}
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
	// A checkpoint recorded under a different UIDVALIDITY belongs to a mailbox
	// since recreated — report "not indexed" (last=0) so the client's catch-up
	// queues a reindex that resets it, rather than trust a stale
	// last_indexed_uid that suppresses indexing of the new low UIDs.
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
		// The checkpoint may sit above the gaps; reset it so the walk revisits
		// the missing range.
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

// enqueueOptimize implements fts.OptimizeNotifier — the engine's write path
// calls this synchronously when a mailbox crosses its shard threshold. It must
// stay fast: optimizeQueue.push takes its own small mutex and returns, no
// compaction happens here.
func (s *Service) enqueueOptimize(user fts.UserRef, mbox fts.MailboxRef) {
	s.optimizeQueue.push(user, mbox)
}

/* --- worker ----------------------------------------------------------------- */

func (s *Service) worker(ctx context.Context) {
	defer s.wg.Done()
	for {
		j, ok := s.queue.pop(ctx)
		if !ok {
			return
		}
		metricQueueDepth.Set(float64(s.queue.depth()))
		t0 := time.Now()
		err := s.runIndex(j)
		metricIndexDuration.Observe(time.Since(t0).Seconds())
		if err != nil {
			metricIndexErrors.Inc()
			slog.Error("fts: index job failed",
				"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "err", err)
			// Recovery: a broken/closed engine handle stays broken for every
			// subsequent job unless reopened. Drop the cached user handle so the
			// next job re-opens a fresh index — the engine also self-heals its
			// write shard, but evicting here recovers even a wholesale-poisoned
			// UserIndex without an operator deleting the on-disk index.
			if reason := brokenEngineReason(err); reason != "" {
				metricRecoveryTotal.WithLabelValues(reason).Inc()
				s.evict(j.user)
				slog.Warn("fts: engine reported a broken index, evicted user handle for reopen",
					"user", j.user, "folder", j.mbox.Name, "reason", reason)
			}
		}
	}
}

// optimizeWorker drains the auto-optimize queue one mailbox at a time: a
// single dedicated goroutine, separate from the index workers, so a long
// compaction never blocks indexing of other users/mailboxes.
func (s *Service) optimizeWorker(ctx context.Context) {
	defer s.wg.Done()
	for {
		j, ok := s.optimizeQueue.pop(ctx)
		if !ok {
			return
		}
		s.runOptimize(j)
		// Cleared only after the run finishes (not on pop): a rotation that
		// happens while this compaction is in flight must be able to queue
		// a fresh pass afterward, since it wasn't covered by this run.
		s.optimizeQueue.done(j.user, j.mbox)
	}
}

func (s *Service) runOptimize(j optimizeJob) {
	h, err := s.handle(j.user.Username)
	if err != nil {
		slog.Warn("fts: auto-optimize could not open user handle",
			"user", j.user.Username, "folder", j.mbox.Name, "err", err)
		return
	}
	if err := s.opts.LockMailbox(j.user.Username, j.mbox.Name, func() error {
		return h.ui.OptimizeMailbox(j.mbox)
	}); err != nil {
		slog.Warn("fts: auto-optimize failed",
			"user", j.user.Username, "folder", j.mbox.Name, "err", err)
	}
}

// brokenEngineReason returns a bounded label when err indicates the engine's
// on-disk index or open handle is unusable (a Xapian DatabaseClosedError, or
// the rev-file open/write failure that wedges a flatcurve shard), "" otherwise.
// A false positive only costs a handle reopen, so the match is deliberately
// broad.
func brokenEngineReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Map each marker to a bounded metric label (fts_recovery_total{reason}).
	for _, m := range []struct{ marker, reason string }{
		{"DatabaseClosedError", "database_closed"},
		{"Database has been closed", "database_closed"},
		{"DatabaseOpeningError", "database_opening"},
		{"Couldn't write new rev file", "rev_file"},
	} {
		if strings.Contains(msg, m.marker) {
			return m.reason
		}
	}
	return ""
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

	// Snapshot the folder (outside the lock): its UIDVALIDITY is the
	// authoritative current value — the Index/autoindex path often sends
	// MailboxRef.UIDValidity=0, so the checkpoint compare must use the folder's
	// own value, not the job's.
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
	// Everything from the checkpoint read through the checkpoint write runs
	// under the per-mailbox lock: concurrent index jobs for the SAME mailbox
	// must not race the read-modify-write of last_indexed_uid and clobber each
	// other's progress. Different mailboxes/users are keyed separately and
	// index in parallel.
	err = s.opts.LockMailbox(j.user, j.mbox.Name, func() error {
		last, storedUIDV, storedSum, cerr := h.ui.Checkpoint(j.mbox)
		if cerr != nil {
			return cerr
		}
		// Decide whether the checkpoint is stale and the index must rebuild:
		//  - settings changed: query-time tokenization no longer matches;
		//  - UIDVALIDITY changed: the mailbox was recreated, so the stale
		//    last_indexed_uid can sit above the new low UIDs and silently
		//    suppress indexing of every new message.
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
			if _, rerr := h.ui.Rescan(j.mbox, nil); rerr != nil { // drop every stale doc
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
		marked := false // folder flagged for heal this scan; gate repeat marks
		for _, m := range msgs {
			if m.UID <= last || m.UID > j.maxUID {
				continue
			}
			if err := s.indexOne(h, j.mbox, m, upd); err != nil {
				var buildErr *buildError
				if errors.As(err, &buildErr) {
					// A hard buildmail failure must never let a partially
					// built document flush into the shard on the NEXT
					// message's first SetBuildKey — Rollback discards it.
					// Commit + checkpoint whatever was fully built before
					// this UID, then halt: the checkpoint must not advance
					// past the failed message, so a future index run
					// (autoindex, delivery, search catch-up) retries it —
					// e.g. after a decoder config fix — instead of silently
					// skipping it forever. Anything reaching here is a hard
					// failure; retriable decoder errors degrade earlier
					// without erroring out of Build.
					return s.haltIndexRunOnBuildFailure(j, h, upd, indexed, curUIDV, checksum, m.UID, buildErr.err)
				}
				skippedCount++
				// One unreadable message must not stall the mailbox forever:
				// log and move the checkpoint past it (rescan can revisit).
				slog.Warn("fts: message skipped",
					"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "uid", m.UID, "err", err)
				// Flag the folder for a reactive heal once per scan, not per
				// message: a mailbox full of vanished files must not pay an
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
	metricIndexMessages.Add(float64(indexedCount))
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
		return &buildError{err: err}
	}
	// Per-message breadcrumb: which UID/file was fed to the engine. Metadata
	// only (size is the index-time signal for "was there anything to
	// tokenize").
	slog.Debug("fts: message indexed", "folder", mbox.Name, "guid", mbox.GUID,
		"uid", m.UID, "file", m.Filename, "size", m.Size, "alt_tier", m.AltTier)
	return nil
}

// buildError tags an indexOne failure as coming from buildmail's Build (a
// content/config problem) rather than Fetch (a storage/read problem). runIndex
// treats these differently: a Build failure halts the run without advancing the
// checkpoint past it (see haltIndexRunOnBuildFailure), while a Fetch failure
// keeps the skip-and-continue-with-heal tolerance.
type buildError struct{ err error }

func (e *buildError) Error() string { return e.err.Error() }
func (e *buildError) Unwrap() error { return e.err }

// haltIndexRunOnBuildFailure discards the partially built document for the
// failed UID so it can never leak into a later message's flush, commits and
// checkpoints whatever was fully built before it, and halts the run — the
// checkpoint does NOT advance past uid, so a future index run retries it once
// the cause is fixed.
//
// This stays loud: a deterministic per-document failure (bad decoder config, a
// permanent 4xx) halts at the same UID on every retry until fixed, so every
// occurrence — not just the first — logs at Error and bumps
// metricIndexBuildHalts. A stuck mailbox must surface as a rising counter and
// a repeating log line, not a single message that scrolls by once.
func (s *Service) haltIndexRunOnBuildFailure(j job, h *userHandle, upd fts.Update, indexed, curUIDV, checksum uint32, uid uint32, buildErr error) error {
	if rerr := upd.Rollback(); rerr != nil {
		slog.Error("fts: rollback after build failure also failed",
			"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "uid", uid, "err", rerr)
	}
	if err := upd.Commit(); err != nil {
		return err
	}
	if err := h.ui.SetCheckpoint(j.mbox, indexed, curUIDV, checksum); err != nil {
		return err
	}
	metricIndexBuildHalts.Inc()
	slog.Error("fts: message build failed, halting mailbox index run without advancing past it",
		"job_id", j.id, "user", j.user, "folder", j.mbox.Name, "uid", uid, "last_good_uid", indexed, "err", buildErr)
	return fmt.Errorf("ftsservice: build uid %d: %w", uid, buildErr)
}
