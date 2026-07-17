package imap

import (
	"errors"
	"io"
	"log/slog"

	"github.com/0kaba0hub/yarilo/internal/storage/idxrebuild"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// reactiveRebuilder is implemented by index-authoritative drivers (dbox) that
// can rebuild a folder index from storage on demand. The trigger is the
// persisted FSCKD marker, set when a read hit a missing/corrupt message.
type reactiveRebuilder interface {
	RebuildFolderIndex(idx mailbox.UserIndex, folder *mailbox.Folder) (idxrebuild.Stats, error)
}

// flagCorruptOnRead persists the folder's FSCKD marker when a read failed
// because the backing storage is missing/corrupt (never for a transient I/O
// error). The next SELECT then rebuilds the index. Best-effort: a failure to
// record the marker is logged, not surfaced to the client.
func (s *session) flagCorruptOnRead(idx mailbox.UserIndex, folderID uint64, folder, filename string, uid uint32, err error) {
	if err == nil || !errors.Is(err, mailbox.ErrCorruptStorage) {
		return
	}
	if merr := idx.MarkFolderCorrupt(folderID); merr != nil {
		slog.Warn("imap: mark folder corrupt failed", "folder", folder, "err", merr)
		return
	}
	slog.Warn("imap: corrupt message flagged for reactive rebuild",
		"folder", folder, "uid", uid, "file", filename, "err", err)
}

// fetchSelected reads a message body from the selected folder, flagging the
// folder for a reactive rebuild if the read tripped over corrupt storage. All
// selected-folder FETCH body reads go through here so the marker is set no
// matter which body specifier triggered the read.
func (s *session) fetchSelected(m *mailbox.MessageMeta) (rc io.ReadCloser, err error) {
	rc, err = s.folderBox().Fetch(s.folder.Name, m.Filename, m.AltTier)
	if err != nil {
		s.flagCorruptOnRead(s.folderIdx(), s.folder.ID, s.folder.Name, m.Filename, m.UID, err)
	}
	return rc, err
}

// dboxRebuildIfCorrupt runs the reactive rebuild when the folder carries the
// FSCKD marker and the driver supports it. Returns a refreshed folder handle
// when a rebuild ran, or nil otherwise. Non-fatal on error.
func (s *session) dboxRebuildIfCorrupt(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	if !s.srv.opts.DboxReactiveRebuild || !f.Fsckd {
		return nil
	}
	rb, ok := h.box.(reactiveRebuilder)
	if !ok {
		return nil
	}
	st, err := rb.RebuildFolderIndex(h.idx, f)
	if err != nil {
		slog.Warn("imap: dbox reactive rebuild failed", "folder", rel, "err", err)
		return nil
	}
	if err := h.idx.ClearFolderCorrupt(f.ID); err != nil {
		slog.Warn("imap: clear corrupt marker failed", "folder", rel, "err", err)
	}
	slog.Info("imap: dbox reactive rebuild", "folder", rel,
		"scanned", st.Scanned, "preserved", st.UIDsPreserved,
		"assigned", st.UIDsAssigned, "dropped", st.OrphansDropped)
	refreshed, err := h.idx.OpenFolder(rel, f.UIDValidity)
	if err != nil {
		slog.Warn("imap: reopen after rebuild failed", "folder", rel, "err", err)
		return nil
	}
	return refreshed
}
