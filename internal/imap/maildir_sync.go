package imap

import (
	"log/slog"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// proactiveSyncer is implemented by mailbox drivers whose on-disk
// representation can change out of band — a message delivered by an MDA into
// new/, or a second MUA moving/removing/renaming a file. Index-authoritative
// drivers (dbox) deliberately do not implement it: they self-heal reactively.
type proactiveSyncer interface {
	ProactiveScan() bool
	// SyncToken is a cheap change-detector for the folder's storage; an
	// unchanged token since the last reconcile means nothing needs doing.
	SyncToken(folder string) string
	// ReconcileIndex imports new files, migrates new/→cur/, expunges vanished
	// ones and repoints renamed files under the driver's own mailbox lock.
	ReconcileIndex(idx mailbox.UserIndex, folder *mailbox.Folder) (mailbox.SyncStats, error)
}

// reconcileFolder runs the proactive reconcile for a folder when the driver
// supports it and the on-disk token changed since this session last synced the
// folder. It returns true when the record set changed, so the caller can
// refresh its view. The token is cached only after a successful pass so a
// failure does not wedge the folder into a permanent skip.
//
// Failures are non-fatal: the caller proceeds against the index as-is so a
// transient scan or lock error never makes a mailbox unusable.
func (s *session) reconcileFolder(h *nsHandle, rel string) bool {
	if !s.srv.opts.MaildirSyncOnSelect {
		return false
	}
	ps, ok := h.box.(proactiveSyncer)
	if !ok || !ps.ProactiveScan() {
		return false
	}
	token := ps.SyncToken(rel)
	if token != "" {
		if prev, seen := s.maildirSyncTokens[rel]; seen && prev == token {
			return false
		}
	}
	f, err := h.idx.OpenFolder(rel, 0)
	if err != nil {
		slog.Warn("imap: reconcile open folder failed", "folder", rel, "err", err)
		return false
	}
	st, err := ps.ReconcileIndex(h.idx, f)
	if err != nil {
		slog.Warn("imap: maildir reconcile failed", "folder", rel, "err", err)
		return false
	}
	if s.maildirSyncTokens == nil {
		s.maildirSyncTokens = make(map[string]string)
	}
	s.maildirSyncTokens[rel] = token
	if !st.Changed {
		return false
	}
	slog.Info("imap: maildir reconcile", "folder", rel,
		"imported", st.Imported, "expunged", st.Expunged, "updated", st.Updated)
	return true
}

// maildirSyncOnSelect reconciles before a SELECT/EXAMINE builds its view and
// returns a refreshed folder handle when the record set changed (so the SELECT
// response reports the new UIDNEXT / HIGHESTMODSEQ), or nil when nothing was
// done.
func (s *session) maildirSyncOnSelect(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	if !s.reconcileFolder(h, rel) {
		return nil
	}
	refreshed, err := h.idx.OpenFolder(rel, f.UIDValidity)
	if err != nil {
		slog.Warn("imap: reopen after sync failed", "folder", rel, "err", err)
		return nil
	}
	return refreshed
}
