package file

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// mailRootDir is where the messages are, as opposed to where our index is. The
// two are different questions: our index follows INDEX=, their files sit with
// the mail, and the mdbox driver roots the mail one level below the home.
func (u *userIndex) mailRootDir() string {
	return dboxconv.StoreRoot(u.home, u.mailPath)
}

// foreignFolderDir is where another implementation would have kept this
// folder's index: with the messages, in the folder's own directory. Ours may be
// somewhere else entirely, which is the reason this is computed from the mail
// root rather than from fs.indexDir.
func (u *userIndex) foreignFolderDir(folder string) string {
	return filepath.Join(u.mailRootDir(),
		mailbox.FolderSubpathEscaped(u.driver, folder, folder, u.separator, u.escapeChar))
}

// convertForeignFolder builds this folder's index from another implementation's,
// in place, and reports whether it did.
//
// Called where a fresh empty index would otherwise be created, and under the
// same lock, so the whole conversion -- read theirs, write ours, remove theirs
// -- is one critical section. Two sessions opening a folder at once convert it
// once, and no session can observe a half-removed pair (#1524).
//
// mdbox only. Their sdbox store keeps each message in its own file inside the
// folder, with no map to convert and a different question to answer about
// naming; refusing here is how that stays a separate piece of work rather than
// a half-done one.
func (u *userIndex) convertForeignFolder(fs *folderState) (bool, error) {
	if u.driver != "mdbox" {
		return false, nil
	}
	dir := u.foreignFolderDir(fs.folder)
	if !dboxconv.HasForeignFolder(dir) {
		return false, nil
	}
	storage := filepath.Join(u.mailRootDir(), "storage")
	if !dboxconv.HasForeignMap(storage) {
		// A folder of theirs with no map of theirs: the messages it names
		// cannot be located at all. Refused rather than converted into an empty
		// folder, which is the one outcome nobody checks.
		return false, fmt.Errorf("fileindex/convert: folder %q has a foreign index and %s has no foreign map",
			fs.folder, storage)
	}

	var opts []mdboxmap.Option
	if u.b.locker != nil {
		opts = append(opts, mdboxmap.WithLocker(u.b.locker), mdboxmap.WithOwner(u.owner))
	}
	m, err := mdboxmap.Open(storage, u.username, opts...)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: open map: %w", err)
	}
	defer m.Close() //nolint:errcheck

	// The map is store-wide and has to be whole before any folder reads through
	// it. Converting it here, on the first folder that needs it, keeps the
	// store's shape correct without a separate pass.
	//
	// The folder lock this runs under is per folder, so it does not order two
	// sessions opening two different folders. The map's own lock does, and
	// ConvertMap decides whether to import inside it: without that both
	// sessions find an empty map and import their whole store twice.
	n, err := dboxconv.ConvertMap(storage, m)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: map: %w", err)
	}
	if n > 0 {
		slog.Info("fileindex: converted a foreign map", "user", u.username, "records", n, "storage", storage)
	}

	corr, err := dboxconv.NewMapCorrespondence(storage, m)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: pair maps: %w", err)
	}
	metas, hdr, err := dboxconv.ConvertFolder(dir, corr)
	if err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}

	// Their UID space, kept whole: the same UIDVALIDITY, the same UIDs, and
	// their next_uid rather than one past the highest surviving message. A
	// client reconnecting over this mailbox then finds what it left (#1568).
	//
	// The uidValidity the opener asked for plays no part, which is why it is
	// not a parameter here: a folder that exists in their store is not a new
	// folder, and the only right answer is the one their index carries.
	if hdr.UIDValidity == 0 {
		return false, fmt.Errorf("fileindex/convert: folder %q: their index carries no uid_validity", fs.folder)
	}
	if err := fs.createFresh(hdr.UIDValidity); err != nil {
		return false, err
	}
	for _, meta := range metas {
		if err := fs.appendLocked(meta); err != nil {
			return false, fmt.Errorf("fileindex/convert: folder %q uid %d: %w", fs.folder, meta.UID, err)
		}
	}
	// next_uid after the appends, which raise it to the highest uid plus one.
	// Theirs can be higher -- a message appended and then expunged moved their
	// counter and left nothing behind -- and reusing that number would hand a
	// client a uid it has already seen carrying different mail.
	if hdr.NextUID > fs.file.Header.NextUID {
		fs.file.Header.NextUID = hdr.NextUID
	}
	// Ours durable before theirs is unlinked. The lock orders sessions; this
	// orders the disk, and a crash between the two has to leave a folder one of
	// the two servers can still open.
	//
	// Both halves are needed and neither substitutes for the other: the file's
	// own bytes have to reach the disk before the rename, and the directory
	// entry has to reach it before the unlink. An index flush is not durable by
	// default anywhere else, because everywhere else the state still exists
	// somewhere if the tail is lost.
	fs.fsyncOnFlush = true
	defer func() { fs.fsyncOnFlush = false }()
	if err := fs.flush(true); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	if err := fsyncDir(fs.indexDir); err != nil {
		return false, fmt.Errorf("fileindex/convert: folder %q: %w", fs.folder, err)
	}
	if err := dboxconv.RemoveForeignFolder(dir); err != nil {
		return false, err
	}
	slog.Info("fileindex: converted a foreign folder", "user", u.username, "folder", fs.folder,
		"messages", len(metas), "from", dir)
	return true, nil
}

// fsyncDir flushes a directory entry, so a file written inside it is found
// again after a crash rather than merely having been written.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	defer d.Close() //nolint:errcheck
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", dir, err)
	}
	return nil
}
