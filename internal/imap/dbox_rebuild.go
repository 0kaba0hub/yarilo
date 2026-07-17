package imap

import (
	"errors"
	"io"
	"log/slog"

	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// flagCorruptOnRead persists the folder's FSCKD marker when a read failed
// because the backing storage is missing/corrupt (never for a transient I/O
// error). The next open then heals the index. Gated per session so a FETCH over
// N corrupt messages does not pay N lock/log round-trips — one mark suffices,
// the heal removes every missing record at once. Best-effort.
func (s *session) flagCorruptOnRead(idx mailbox.UserIndex, folderID uint64, folder, filename string, uid uint32, err error) {
	if err == nil || !errors.Is(err, mailbox.ErrCorruptStorage) {
		return
	}
	cm, ok := idx.(mailbox.CorruptionMarker)
	if !ok {
		return
	}
	if s.markedCorrupt == nil {
		s.markedCorrupt = make(map[string]bool)
	}
	if s.markedCorrupt[folder] {
		return
	}
	if merr := cm.MarkFolderCorrupt(folderID); merr != nil {
		slog.Warn("imap: mark folder corrupt failed", "folder", folder, "err", merr)
		return
	}
	s.markedCorrupt[folder] = true
	slog.Warn("imap: corrupt message flagged for reactive heal",
		"folder", folder, "uid", uid, "file", filename, "err", err)
}

// fetchSelected reads a message body from the selected folder, flagging the
// folder for a reactive heal if the read tripped over corrupt storage. All
// selected-folder FETCH body reads go through here so the marker is set no
// matter which body specifier triggered the read.
func (s *session) fetchSelected(m *mailbox.MessageMeta) (rc io.ReadCloser, err error) {
	rc, err = s.folderBox().Fetch(s.folder.Name, m.Filename, m.AltTier)
	if err != nil {
		// Only flag corruption a driver can actually heal: a driver without a
		// reactive rebuilder (mdbox until #594 Phase 2b) would otherwise be left
		// stuck FSCKD with nothing to clear the marker.
		if mailbox.CanReactiveHeal(s.folderBox()) {
			s.flagCorruptOnRead(s.folderIdx(), s.folder.ID, s.folder.Name, m.Filename, m.UID, err)
		}
	}
	return rc, err
}

// dboxHealIfCorrupt runs the reactive heal when the folder carries the FSCKD
// marker and the driver supports it. Returns a refreshed folder handle when a
// heal ran, or nil otherwise. Non-fatal on error. Used from SELECT, STATUS and
// Poll/IDLE so a flagged folder heals on whichever the client hits first.
func (s *session) dboxHealIfCorrupt(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	if !s.srv.opts.DboxReactiveRebuild {
		return nil
	}
	if !f.Fsckd {
		// The folder is clean — possibly because another session already healed
		// and cleared the marker. Drop our stale per-session flag so a fresh
		// corruption re-flags this folder instead of being suppressed until the
		// session ends.
		delete(s.markedCorrupt, rel)
		return nil
	}
	rb, ok := h.box.(mailbox.ReactiveHealer)
	if !ok {
		return nil
	}
	expunged, err := rb.HealCorruptFolder(h.idx, f)
	if err != nil {
		slog.Warn("imap: dbox reactive heal failed", "folder", rel, "err", err)
		return nil
	}
	// The marker is cleared, so drop any per-session mark so a later corruption
	// re-flags the folder.
	delete(s.markedCorrupt, rel)
	slog.Info("imap: dbox reactive heal", "folder", rel, "expunged", expunged)
	refreshed, err := h.idx.OpenFolder(rel, f.UIDValidity)
	if err != nil {
		slog.Warn("imap: reopen after heal failed", "folder", rel, "err", err)
		return nil
	}
	return refreshed
}
