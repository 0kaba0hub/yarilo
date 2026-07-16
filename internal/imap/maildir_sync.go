package imap

import (
	"log/slog"

	"github.com/0kaba0hub/yarilo/internal/storage/reconcile"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// proactiveSyncer is implemented by mailbox drivers whose on-disk
// representation can change out of band — a message delivered by an MDA into
// new/, or a second MUA moving/removing a file. Index-authoritative drivers
// (dbox) deliberately do not implement it: they self-heal reactively instead.
type proactiveSyncer interface {
	ProactiveScan() bool
	// SyncToken is a cheap change-detector for the folder's storage; an
	// unchanged token since the last SELECT means no reconcile is needed.
	SyncToken(folder string) string
	// ReconcileIndex imports new files and expunges vanished ones under the
	// driver's own mailbox lock, leaving tracked messages untouched.
	ReconcileIndex(idx mailbox.UserIndex, folder *mailbox.Folder) (reconcile.Stats, error)
}

// maildirSyncOnSelect reconciles the index against physical storage when the
// driver supports proactive scanning and the folder's on-disk token changed
// since this session last synced it. It returns a refreshed folder handle when
// the reconcile changed the record set (so the SELECT response reports the new
// UIDNEXT / HIGHESTMODSEQ), or nil when nothing was done.
//
// Failures are non-fatal: SELECT proceeds against the index as-is so a
// transient scan or lock error never makes a mailbox unopenable.
func (s *session) maildirSyncOnSelect(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	if !s.srv.opts.MaildirSyncOnSelect {
		return nil
	}
	ps, ok := h.box.(proactiveSyncer)
	if !ok || !ps.ProactiveScan() {
		return nil
	}
	token := ps.SyncToken(rel)
	if token != "" {
		if prev, seen := s.maildirSyncTokens[rel]; seen && prev == token {
			return nil
		}
	}
	st, err := ps.ReconcileIndex(h.idx, f)
	if err != nil {
		slog.Warn("imap: maildir sync-on-select failed", "folder", rel, "err", err)
		return nil
	}
	if s.maildirSyncTokens == nil {
		s.maildirSyncTokens = make(map[string]string)
	}
	s.maildirSyncTokens[rel] = token
	if !st.Changed {
		return nil
	}
	slog.Info("imap: maildir sync-on-select", "folder", rel,
		"imported", st.Imported, "expunged", st.Expunged)
	refreshed, err := h.idx.OpenFolder(rel, f.UIDValidity)
	if err != nil {
		slog.Warn("imap: reopen after sync failed", "folder", rel, "err", err)
		return nil
	}
	return refreshed
}
