package imap

import (
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// createFolderIndex writes a new folder's index at the moment the folder is
// created, saying that is what it is.
//
// Creating a folder used to make only its directory; the index appeared later,
// on whichever open touched it first. That left the two states -- a folder just
// created and a folder whose index was lost -- looking identical on disk, and
// the open that had to serve both served the second as the first: an empty
// mailbox over a directory of mail (#1608).
//
// Best-effort by design: an index that cannot be written here is written by the
// next open exactly as before, so a failure costs the distinction and not the
// folder.
func createFolderIndex(idx mailbox.UserIndex, folder string, uidValidity uint32) {
	fc, ok := idx.(mailbox.FolderCreator)
	if !ok {
		return
	}
	if _, err := fc.CreateFolder(folder, uidValidity); err != nil {
		slog.Warn("imap: folder index not created with the folder", "folder", folder, "err", err)
	}
}
