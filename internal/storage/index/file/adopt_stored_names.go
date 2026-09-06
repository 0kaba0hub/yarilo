package file

import (
	"fmt"
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// AdoptStoredNames hands each record's stored name and guid to the caller for
// its storage key, keeps the answer, and marks the folder (#1700).
func (u *userIndex) AdoptStoredNames(folderID uint64, keyOf func(name string, guid [16]byte) (uint32, bool)) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if uidNamedLocked(fs) {
			return nil
		}
		stamped := 0
		for _, rec := range fs.file.Records {
			if mapUID, _ := decodeMdboxRec(rec.Ext[extNameMdbox]); mapUID != 0 {
				continue
			}
			key, ok := keyOf(fs.filenames[rec.UID], decodeGUIDRec(rec.Ext[extNameGUID]))
			if !ok {
				continue
			}
			if rec.Ext == nil {
				rec.Ext = map[string][]byte{}
			}
			rec.Ext[extNameMdbox] = encodeMdboxRec(key, 0)
			stamped++
		}
		if stamped > 0 {
			fs.ensureMdboxExtLocked()
		}
		if err := fs.markUIDNamedLocked(); err != nil {
			return err
		}
		if err := fs.flush(true); err != nil {
			return fmt.Errorf("fileindex/adopt: %q: %w", fs.folder, err)
		}
		if err := fs.dropSidecarLocked(); err != nil {
			return err
		}
		slog.Info("fileindex: the names are in the records now",
			"user", fs.user, "folder", fs.folder, "stamped", stamped)
		return nil
	})
}

var _ mailbox.StoredNameAdopter = (*userIndex)(nil)
