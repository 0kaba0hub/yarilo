package imap

import (
	"errors"
	"io"
	"log/slog"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// flagCorruptOnRead persists the folder's FSCKD marker when a read failed on
// missing/corrupt storage (never on a transient I/O error) so the next open
// heals the index. Gated per session so a FETCH over N corrupt messages pays
// one lock/log round-trip, not N. Best-effort.
func (s *session) flagCorruptOnRead(idx mailbox.UserIndex, folderID uint64, folder, filename string, uid uint32, err error) {
	if err == nil || !errors.Is(err, mailbox.ErrCorruptStorage) {
		return
	}
	cm, ok := idx.(mailbox.CorruptionMarker)
	if !ok {
		return
	}
	// Re-read the index: a record another session expunged between this
	// session's snapshot and the read is not corruption (#1690).
	if !s.recordStillThere(idx, folderID, uid) {
		return
	}
	if s.markedCorrupt == nil {
		s.markedCorrupt = make(map[uint64]bool)
	}
	// Key by folder ID, not name: the mark site (FETCH uses s.folder.Name) and
	// the clear site (SELECT/STATUS use the namespace-relative name) differ for
	// shared/public folders; the ID is the one identity every call site has.
	if s.markedCorrupt[folderID] {
		return
	}
	if merr := cm.MarkFolderCorrupt(folderID); merr != nil {
		slog.Warn("imap: mark folder corrupt failed", "user", s.username(), "folder", folder, "err", merr)
		return
	}
	s.markedCorrupt[folderID] = true
	slog.Warn("imap: corrupt message flagged for reactive heal",
		"user", s.username(), "folder", folder, "uid", uid, "file", filename, "err", err)
}

// recordStillThere reports whether the folder's index still carries uid. A read
// error on a record another session has expunged is that expunge, not damage.
func (s *session) recordStillThere(idx mailbox.UserIndex, folderID uint64, uid uint32) bool {
	msgs, err := idx.GetMessages(folderID, mailbox.SeqSet{{From: uid, To: uid}})
	if err != nil {
		// Unknown: keep the old behaviour rather than swallow a real fault.
		return true
	}
	for _, m := range msgs {
		if m.UID == uid {
			return true
		}
	}
	return false
}

// fetchSelected reads a message body from the selected folder, flagging the
// folder for a reactive heal if the read tripped over corrupt storage. All
// selected-folder FETCH body reads go through here so the marker is set
// whichever body specifier triggered the read.
func (s *session) fetchSelected(m *mailbox.MessageMeta) (rc io.ReadCloser, err error) {
	rc, err = s.folderBox().Fetch(s.folder.Name, m.Filename, m.AltTier)
	if err != nil {
		// Only flag corruption a driver can actually heal; a driver without a
		// reactive rebuilder would otherwise be stuck FSCKD with nothing to
		// clear the marker.
		if mailbox.CanReactiveHeal(s.folderBox()) {
			s.flagCorruptOnRead(s.folderIdx(), s.folder.ID, s.folder.Name, m.Filename, m.UID, err)
		}
	}
	return rc, err
}

// dboxHealIfCorrupt runs the reactive heal when the folder carries the FSCKD
// marker and the driver supports it. Returns a refreshed folder handle when a
// heal ran, else nil. Non-fatal on error. Used from SELECT, STATUS and
// Poll/IDLE so a flagged folder heals on whichever the client hits first.
func (s *session) dboxHealIfCorrupt(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	if !s.srv.opts.DboxReactiveRebuild {
		return nil
	}
	if !f.Fsckd {
		// Clean, possibly because another session already healed. Drop the
		// stale per-session flag so a fresh corruption re-flags this folder
		// instead of being suppressed until the session ends.
		delete(s.markedCorrupt, f.ID)
		return nil
	}
	rb, ok := mailbox.Driver(h.box).(mailbox.ReactiveHealer)
	if !ok {
		return nil
	}
	expunged, err := rb.HealCorruptFolder(h.idx, f)
	if errors.Is(err, mdbox.ErrHealDeferred) {
		// Already failed at this storage generation: a reconnect is not new
		// evidence, and each attempt costs a whole-storage scan (#1682).
		slog.Debug("imap: dbox reactive heal deferred", "user", s.username(), "folder", rel)
		return nil
	}
	if err != nil {
		slog.Warn("imap: dbox reactive heal failed", "user", s.username(), "folder", rel, "err", err)
		return nil
	}
	// Marker cleared: drop the per-session mark so a later corruption re-flags
	// the folder.
	delete(s.markedCorrupt, f.ID)
	// Invalidate FTS documents for the expunged records; otherwise ghost
	// documents linger until the next fts rescan.
	for _, uid := range expunged {
		s.ftsNotify(f, true, uid)
	}
	slog.Info("imap: dbox reactive heal", "user", s.username(), "folder", rel, "expunged", len(expunged))
	refreshed, err := h.idx.OpenFolder(rel, f.UIDValidity)
	if err != nil {
		slog.Warn("imap: reopen after heal failed", "user", s.username(), "folder", rel, "err", err)
		return nil
	}
	return refreshed
}

// dboxRestoreIfIndexLost reimports a dbox folder whose index is gone while its
// messages are still in storage, and returns a refreshed handle when it did.
//
// Losing the folder index used to mean losing the mailbox: the open created a
// fresh empty index beside a directory full of mail, so the folder answered
// 0 EXISTS with a new UIDVALIDITY, and nothing anywhere said the messages were
// right there. maildir has always had this repair -- its proactive sync
// reimports from disk -- and dbox had none: the reactive heal fires on a read
// that trips over corrupt storage, and a folder with no records has no read to
// trip (#1608).
//
// Only when the index is empty and the storage is not. Both empty is an
// ordinary new folder and must stay one.
//
// UIDVALIDITY cannot be brought back: it lived in the index that was lost, so
// the rebuild's fresh one stands and every client resynchronises. That is the
// cost of the repair, not a reason to withhold it -- the alternative on the
// table is a mailbox that reads as empty.
func (s *session) dboxRestoreIfIndexLost(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	if f.Messages > 0 {
		return nil
	}
	box := mailbox.Driver(h.box)
	// Storage-wide scanners are excluded here on purpose. Their repair is the
	// storage-wide rebuild, which its own contract says must run with the
	// user's mailboxes quiesced -- firing it from a folder open can reclaim
	// live mail out from under a concurrent delivery (#1608).
	if fa, ok := box.(mailbox.FolderAgnosticStorage); ok && fa.FolderAgnosticScan() {
		return nil
	}
	if _, ok := box.(mailbox.ReactiveHealer); !ok {
		// A dbox driver, which is what this is for; maildir repairs itself
		// through its own sync.
		return nil
	}
	recs, err := h.box.Scan(rel)
	if err != nil || len(recs) == 0 {
		return nil
	}
	slog.Warn("imap: folder index is missing and its messages are in storage; rebuilding from the files",
		"user", s.username(), "folder", rel, "files", len(recs))
	st, err := idxrebuild.RebuildFolder(h.box, h.idx, f)
	if err != nil {
		slog.Warn("imap: rebuild after index loss failed", "user", s.username(), "folder", rel, "err", err)
		return nil
	}
	refreshed, err := h.idx.OpenFolder(rel, f.UIDValidity)
	if err != nil {
		slog.Warn("imap: reopen after rebuild failed", "user", s.username(), "folder", rel, "err", err)
		return nil
	}
	slog.Info("imap: rebuilt a folder from storage after index loss",
		"user", s.username(), "folder", rel,
		"messages", st.UIDsAssigned+st.UIDsPreserved, "uidvalidity", refreshed.UIDValidity)
	return refreshed
}
