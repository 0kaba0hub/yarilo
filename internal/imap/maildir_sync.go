package imap

import (
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
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

// syncTokens returns the token cache this session reconciles against: the
// process-wide one, unless a test wired its own.
func (s *session) syncTokens() *syncTokenCache {
	if s.maildirSyncTokens != nil {
		return s.maildirSyncTokens
	}
	return maildirSyncTokens
}

// username is the identity the token cache is keyed by; empty before login,
// which cannot reach a reconcile.
func (s *session) username() string {
	if s.userInfo == nil {
		return ""
	}
	return s.userInfo.Username
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
	ps, ok := mailbox.Driver(h.box).(proactiveSyncer)
	if !ok || !ps.ProactiveScan() {
		return false
	}
	start := time.Now()
	key := syncTokenKey(s.username(), h.location, rel)
	token := ps.SyncToken(rel)
	if token != "" {
		if prev, seen := s.syncTokens().get(key); seen && prev == token {
			metricMaildirSync.WithLabelValues("skipped").Inc()
			metricMaildirSyncSeconds.Observe(time.Since(start).Seconds())
			return false
		}
	}
	metricMaildirSync.WithLabelValues("scanned").Inc()
	defer func() { metricMaildirSyncSeconds.Observe(time.Since(start).Seconds()) }()
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
	// Cached only after a successful pass, so a scan or lock failure does not
	// wedge the folder into a permanent skip.
	s.syncTokens().put(key, token)
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
